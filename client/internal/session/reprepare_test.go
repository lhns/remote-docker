package session

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/dircache"
)

const (
	reprepareClient  = "aabbccdd"
	reprepareUnknown = `{"err":"unions: /m/x has no cache: no union for this share","unknown":true}`
)

// reprepareChannel is a channel answering with the given lines, wired to a
// session holding one write=back share owned by 1000:1000, as ensureCacheChan
// wires it. It returns the share's export and cache volume.
func reprepareChannel(t *testing.T, lines ...string) (*cacheChannel, *scripted, string, string) {
	t.Helper()
	s := hostingSession(t, workspace.WatchOff, nil)
	share, err := s.registry.Register(t.TempDir(),
		workspace.Mode{Read: workspace.ReadCached, Write: workspace.WriteBack},
		workspace.ShareWords{Owner: workspace.Owner{UID: 1000, GID: 1000, Set: true}})
	if err != nil {
		t.Fatal(err)
	}
	volume, err := workspace.CacheVolumeForExport(reprepareClient, share.ExportPath)
	if err != nil {
		t.Fatal(err)
	}

	script := make([]string, len(lines))
	for i, l := range lines {
		script[i] = strings.ReplaceAll(l, "$VOLUME", volume)
	}
	stream := newScripted(strings.Join(script, "\n") + "\n")
	c := channelOver(stream)
	c.owner = true
	live := &liveConn{clientID: reprepareClient, info: workspace.Info{NFSPort: 30001}}
	c.reprepare = func(ctx context.Context, export string) error { return s.reprepare(ctx, live, c, export) }
	return c, stream, share.ExportPath, volume
}

// sent is every request a scripted channel was sent, in order.
func sent(t *testing.T, stream *scripted) []cache.Request {
	t.Helper()
	var out []cache.Request
	for _, line := range strings.Split(strings.TrimSpace(string(stream.written)), "\n") {
		var req cache.Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("not a request: %q", line)
		}
		out = append(out, req)
	}
	return out
}

func ops(reqs []cache.Request) []cache.Op {
	out := make([]cache.Op, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Op)
	}
	return out
}

// An agent restarted under a running container has no record of the union that
// container still binds. Write-back used to take that for a released union and
// stop for the share; the union is prepared again, which the agent answers by
// adopting it, and the request is asked once more.
func TestAUnionTheWorkspaceStillMountsIsPreparedAgain(t *testing.T) {
	c, stream, export, volume := reprepareChannel(t,
		reprepareUnknown,
		`{"v":["$VOLUME"]}`,
		`{"m":"/run/rd-union/x/merged"}`,
		`{"c":[{"p":"/written-there","s":5}]}`,
	)

	changes, err := c.Changes(t.Context(), export)
	if err != nil {
		t.Fatalf("Changes = %v, want the container's changes once the union was prepared again", err)
	}
	if len(changes) != 1 || changes[0].Path != "/written-there" {
		t.Errorf("changes = %+v, want the one the retried request answered", changes)
	}

	reqs := sent(t, stream)
	want := []cache.Op{cache.OpChanges, cache.OpMounted, cache.OpPrepare, cache.OpChanges}
	if got := ops(reqs); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	prep := reqs[2]
	if prep.Export != export || prep.Cache != volume || prep.Port != 30001 ||
		prep.Read != string(workspace.ReadCached) || prep.Owner != "1000:1000" {
		t.Errorf("prepare = %+v, want the share's export, cache volume, port, read mode and owner", prep)
	}
}

// What cannot be given back is forgotten as before, and never asked twice.
func TestAUnionThatCannotBePreparedAgainIsGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		want  []cache.Op
	}{
		{
			// Released because no container held it: preparing it would mount
			// a union for nobody.
			name:  "not mounted",
			lines: []string{reprepareUnknown, `{"v":["rd-other-cache"]}`},
			want:  []cache.Op{cache.OpChanges, cache.OpMounted},
		},
		{
			name:  "prepare refused",
			lines: []string{reprepareUnknown, `{"v":["$VOLUME"]}`, `{"err":"unions: the union did not come up"}`},
			want:  []cache.Op{cache.OpChanges, cache.OpMounted, cache.OpPrepare},
		},
		{
			name:  "still unknown",
			lines: []string{reprepareUnknown, `{"v":["$VOLUME"]}`, `{"m":"/run/rd-union/x/merged"}`, reprepareUnknown},
			want:  []cache.Op{cache.OpChanges, cache.OpMounted, cache.OpPrepare, cache.OpChanges},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, stream, export, _ := reprepareChannel(t, tc.lines...)
			if _, err := c.Changes(t.Context(), export); !errors.Is(err, dircache.ErrShareGone) {
				t.Fatalf("Changes = %v, want ErrShareGone", err)
			}
			if got := ops(sent(t, stream)); !slices.Equal(got, tc.want) {
				t.Errorf("requests = %v, want %v", got, tc.want)
			}
		})
	}
}

// A channel that broke while asking says nothing about the share, so the
// caller retries the round rather than forgetting a share a container uses.
func TestABrokenAskIsRetriedNotForgotten(t *testing.T) {
	c, _, export, _ := reprepareChannel(t, reprepareUnknown)
	_, err := c.Changes(t.Context(), export)
	if err == nil || errors.Is(err, dircache.ErrShareGone) {
		t.Fatalf("Changes = %v, want the broken channel's error", err)
	}
}
