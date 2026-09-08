package rewrite

import (
	"os"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// What REMOTE_DOCKER_NFS_NCONNECT accepts, and what it refuses rather than
// carrying into a mount option the NFS client would reject along with the rest
// of the option string.
func TestNConnect(t *testing.T) {
	for _, c := range []struct {
		env     string
		set     bool
		want    int
		refused bool
	}{
		{set: false, want: 0},
		{env: "", set: true, want: 0},
		{env: "   ", set: true, want: 0},
		{env: "0", set: true, want: 0},
		{env: "1", set: true, want: 1},
		{env: "4", set: true, want: 4},
		{env: " 8 ", set: true, want: 8},
		{env: "16", set: true, want: 16},

		// Refused rather than clamped: a number outside the range is a request
		// nobody can honour, and silently mounting with a different one is how a
		// person concludes the variable does nothing.
		{env: "17", set: true, refused: true},
		{env: "-1", set: true, refused: true},
		{env: "eight", set: true, refused: true},
		{env: "8,4", set: true, refused: true},
	} {
		if c.set {
			t.Setenv(NConnectEnv, c.env)
		} else {
			unsetEnv(t, NConnectEnv)
		}

		got, err := NConnect()
		if c.refused {
			if err == nil {
				t.Errorf("%s=%q gives %d and no error, want a refusal", NConnectEnv, c.env, got)
			} else if !strings.Contains(err.Error(), NConnectEnv) {
				t.Errorf("%s=%q is refused with %q, which does not name the variable", NConnectEnv, c.env, err)
			}
			// A refusal must leave the mount asking for nothing, since the
			// caller logs and carries on with what it got.
			if got != 0 {
				t.Errorf("%s=%q is refused and still gives %d, want 0", NConnectEnv, c.env, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s=%q: %v", NConnectEnv, c.env, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s=%q gives %d, want %d", NConnectEnv, c.env, got, c.want)
		}
	}
}

// What the variable asks for reaches the volume a share is mounted from, and a
// rewriter that was told nothing asks for nothing.
func TestNConnectReachesTheVolume(t *testing.T) {
	const bind = `{"Image":"alpine","HostConfig":{"Binds":["/home/alice/project:/app"]}}`
	volume := workspace.VolumeNameForID("", workspace.ShareID("/home/alice/project"))

	r, _, volumes := newRewriter()
	if _, err := r.ContainerCreate(t.Context(), []byte(bind)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if o := volumes.created[volume]["o"]; strings.Contains(o, "nconnect") {
		t.Errorf("a rewriter asked for no connections built %q", o)
	}

	r, _, volumes = newRewriter()
	r.NConnect = 4
	if _, err := r.ContainerCreate(t.Context(), []byte(bind)); err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if o := volumes.created[volume]["o"]; !strings.Contains(o, "nconnect=4") {
		t.Errorf("volume options %q are missing nconnect=4", o)
	}
}

// The ceiling this refuses at is the kernel's, so the two cannot drift.
func TestNConnectStopsWhereTheKernelDoes(t *testing.T) {
	if workspace.NConnectMax != 16 {
		t.Errorf("NConnectMax is %d, where the NFS client's NFS_MAX_CONNECTIONS is 16 "+
			"(v6.6 fs/nfs/fs_context.c). A mount above it is refused with the whole option string.",
			workspace.NConnectMax)
	}
}

// unsetEnv removes a variable for the length of a test, which t.Setenv cannot
// express and which the "not set at all" row needs.
func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "placeholder")
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unsetting %s: %v", name, err)
	}
}
