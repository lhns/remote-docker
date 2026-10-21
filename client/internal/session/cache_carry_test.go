package session

import (
	"bufio"
	"errors"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/dircache"
)

// scripted is a stream that answers every request with the same bytes and keeps
// what was written to it.
type scripted struct {
	reader  *strings.Reader
	written []byte
	closed  bool
}

func newScripted(reply string) *scripted { return &scripted{reader: strings.NewReader(reply)} }

func (s *scripted) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *scripted) Write(p []byte) (int, error) {
	s.written = append(s.written, p...)
	return len(p), nil
}
func (s *scripted) Close() error { s.closed = true; return nil }

func channelOver(s *scripted) *cacheChannel {
	return newCacheChannel(s, bufio.NewReaderSize(s, cache.MaxFrame))
}

// Only a stream that cannot be trusted retires the channel. The workspace
// refusing a request in words leaves it intact, and reopening on every refusal
// would drop every union the channel prepared.
func TestARefusalLeavesTheChannelUsableAndGarbageDoesNot(t *testing.T) {
	c := channelOver(newScripted(`{"err":"unions: no cache for this share","unknown":true}` + "\n"))
	if _, err := c.Changes(t.Context(), "/m/1111111111111111"); !errors.Is(err, dircache.ErrShareGone) {
		t.Fatalf("Changes = %v, want ErrShareGone", err)
	}
	if !c.usable() {
		t.Error("a share the workspace has no union for retired the whole channel")
	}

	c = channelOver(newScripted(`{"err":"unions: the cache layer cannot be read"}` + "\n"))
	if _, err := c.Changes(t.Context(), "/m/1111111111111111"); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("Changes = %v, want the workspace's words", err)
	}
	if !c.usable() {
		t.Error("a request the workspace refused in words retired the channel")
	}

	c = channelOver(newScripted("\xf0 not json at all\n"))
	if _, err := c.Changes(t.Context(), "/m/1111111111111111"); err == nil {
		t.Fatal("garbage answered a request")
	}
	if c.usable() {
		t.Error("a channel that read garbage is still in use; the next reply would be read from the middle of something")
	}
}

// What the client asks for depends on what the workspace announced: an agent
// without Carry would ignore the request and the client would take a skipped
// entry for a landed one.
func TestCarryIsAskedForOnlyWhenTheAgentAnnouncedIt(t *testing.T) {
	for _, carry := range []bool{false, true} {
		stream := newScripted(`{"c":[{"p":"/tools","o":true},{"p":"/bin/tool","l":"../x"}]}` + "\n")
		c := channelOver(stream)
		c.carry = carry

		changes, err := c.Changes(t.Context(), "/m/1111111111111111")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(string(stream.written), `"y":true`); got != carry {
			t.Errorf("carry=%v: the request %s asked for Carry = %v", carry, stream.written, got)
		}
		if len(changes) != 2 || !changes[0].Opaque || changes[1].Link != "../x" {
			t.Errorf("carry=%v: changes = %+v, want the opaque directory and the link as dircache's", carry, changes)
		}
	}
}

// An apply names what the workspace left out, and an agent that does not skip
// names nothing.
func TestApplyReturnsWhatTheWorkspaceSkipped(t *testing.T) {
	root := t.TempDir()
	stream := newScripted(`{"k":["/b.go"]}` + "\n")
	c := channelOver(stream)
	c.carry = true

	skipped, err := c.Apply(t.Context(), "/m/1111111111111111", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "/b.go" {
		t.Errorf("skipped = %v, want /b.go", skipped)
	}
	if !strings.Contains(string(stream.written), `"y":true`) {
		t.Errorf("the apply %s did not ask the workspace to skip", stream.written)
	}
}

// A dead channel that is replaced is closed: left open it holds an SSH channel
// and the agent's handler for the life of the connection.
func TestReplacingADeadCacheChannelClosesIt(t *testing.T) {
	client := startWorkspace(t, func(string) (string, bool) { return helloFor(t), true })
	old := newScripted("")
	dead := channelOver(old)
	dead.dead.Store(true)
	live := &liveConn{ssh: client, cacheChan: dead}

	fresh, err := (&Session{}).ensureCacheChan(t.Context(), live)
	if err != nil || fresh == dead {
		t.Fatalf("ensureCacheChan = %v, %v; want a new channel", fresh, err)
	}
	if !old.closed {
		t.Error("the dead channel was replaced and left open")
	}
}
