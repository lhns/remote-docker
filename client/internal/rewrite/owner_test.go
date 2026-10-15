package rewrite

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// owner= is a word of the share, in both spellings: a `-v` option list, where
// the gid cannot follow a colon, and `--mount`'s csv-quoted field, where it
// can. It is consumed, and ro stays.
func TestOwnerWordReachesTheShare(t *testing.T) {
	const project = "/home/alice/project"
	for _, c := range []struct {
		name string
		body string
		want workspace.Owner
	}{
		{"-v uid", `{"HostConfig":{"Binds":["` + project + `:/app:ro,owner=1000"]}}`, workspace.Owner{UID: 1000, GID: 1000, Set: true}},
		{"-v dot", `{"HostConfig":{"Binds":["` + project + `:/app:owner=1000.2000,ro"]}}`, workspace.Owner{UID: 1000, GID: 2000, Set: true}},
		{"--mount colon", `{"HostConfig":{"Mounts":[{"Type":"bind","Source":"` + project + `","Target":"/app","Consistency":"owner=1000:2000"}]}}`, workspace.Owner{UID: 1000, GID: 2000, Set: true}},
		{"nothing named, session default", `{"HostConfig":{"Binds":["` + project + `:/app"]}}`, workspace.Owner{UID: 5, GID: 6, Set: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, sharer, _ := newRewriter()
			r.ShareOwner = workspace.Owner{UID: 5, GID: 6, Set: true}

			out, err := r.ContainerCreate(t.Context(), []byte(c.body))
			if err != nil {
				t.Fatalf("ContainerCreate: %v", err)
			}
			if got := sharer.words[project].Owner; got != c.want {
				t.Errorf("shared with owner %v, want %v", got, c.want)
			}
			if strings.Contains(string(out), "owner") {
				t.Errorf("an owner word reached the daemon: %s", out)
			}
			if strings.Contains(c.body, ":ro") && !strings.Contains(string(out), ":/app:ro") {
				t.Errorf("ro was lost: %s", out)
			}
		})
	}

	// No session default either: root.
	r, sharer, _ := newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":["`+project+`:/app"]}}`)); err != nil {
		t.Fatal(err)
	}
	if got := sharer.words[project].Owner; got != (workspace.Owner{Set: true}) {
		t.Errorf("default owner = %v, want 0:0", got)
	}
}

// Mounts of one directory in one request with another owner are another share
// each, with their own volume (ADR 0046). The session default counts as an
// answer; the same owner in two spellings is one share.
func TestOneDirectoryWithTwoOwnersGetsTwoShares(t *testing.T) {
	r, sharer, volumes := newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:owner=1000","/home/alice/project:/also"]}}`)); err != nil {
		t.Fatalf("two owners for one directory were refused: %v", err)
	}
	if len(volumes.created) != 2 || sharer.exports[0] == sharer.exports[1] {
		t.Errorf("volumes = %v, exports = %v; want two of each", volumes.created, sharer.exports)
	}

	r, sharer, volumes = newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(`{"HostConfig":{"Binds":[`+
		`"/home/alice/project:/app:owner=1000","/home/alice/project:/also:owner=1000.1000"]}}`)); err != nil {
		t.Errorf("the same owner in two spellings was refused: %v", err)
	}
	if len(volumes.created) != 1 || sharer.exports[0] != sharer.exports[1] {
		t.Errorf("volumes = %v, exports = %v; want one share", volumes.created, sharer.exports)
	}
}
