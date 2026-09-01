package workspace

import (
	"strings"
	"testing"
)

// The client and the agent both call Validate, and the agent's call is the one
// that matters: this stream tells a root process which paths to write and which
// to remove inside the workspace.
func TestCacheRequestValidate(t *testing.T) {
	const share = "/m/00112233445566ff"
	cache := VolumeNameForID("aabbccdd", "00112233445566ff")

	for _, c := range []struct {
		name string
		req  CacheRequest
		want string // a substring of the refusal, or "" to accept
	}{
		{
			name: "a prepare with a port and a managed volume",
			req:  CacheRequest{Op: OpPrepare, Export: share, Port: 30001, Cache: cache},
		},
		{
			name: "a prepare with no port has no address to mount from",
			req:  CacheRequest{Op: OpPrepare, Export: share, Cache: cache},
			want: "port",
		},
		{
			// The agent would otherwise mount whatever it was handed, inside
			// the account's daemon, as root.
			name: "a prepare naming a volume this client did not create",
			req:  CacheRequest{Op: OpPrepare, Export: share, Port: 30001, Cache: "postgres-data"},
			want: "managed volume",
		},
		{
			name: "an apply",
			req:  CacheRequest{Op: OpApply, Export: share, Bytes: 4096},
		},
		{
			name: "an apply of nothing, which is how an empty batch looks",
			req:  CacheRequest{Op: OpApply, Export: share},
		},
		{
			// The field exists so compression is a negotiation later rather
			// than a new protocol; until then, saying yes to one we do not
			// have would be worse than refusing.
			name: "an apply asking for a codec this version has not got",
			req:  CacheRequest{Op: OpApply, Export: share, Bytes: 10, Codec: "zstd"},
			want: "codec",
		},
		{
			name: "a drop",
			req:  CacheRequest{Op: OpDrop, Export: share, Paths: []string{"/pkg/lib.go"}},
		},
		{
			name: "a drop that names nothing",
			req:  CacheRequest{Op: OpDrop, Export: share},
			want: "no paths",
		},
		{
			name: "a drop that walks out of the share",
			req:  CacheRequest{Op: OpDrop, Export: share, Paths: []string{"/../../etc/shadow"}},
			want: "component",
		},
		{
			// Removing the share root is unmounting it, which is a different
			// request with a different name.
			name: "a drop that names the share root",
			req:  CacheRequest{Op: OpDrop, Export: share, Paths: []string{"/"}},
			want: "root",
		},
		{
			name: "a release",
			req:  CacheRequest{Op: OpRelease, Export: share},
		},
		{
			name: "an export that is not one this program serves",
			req:  CacheRequest{Op: OpRelease, Export: "/etc"},
			want: "export",
		},
		{
			name: "an op from a client newer than this agent",
			req:  CacheRequest{Op: "promote", Export: share},
			want: "unknown op",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.req.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Errorf("Validate() = %v, want it accepted", err)
			case c.want != "" && err == nil:
				t.Errorf("Validate() accepted %+v", c.req)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Errorf("Validate() = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// The cwd share is a share like any other, and the commonest one of all: it is
// what `-v .:/app` becomes.
func TestCacheRequestAcceptsTheWorkingDirectoryShare(t *testing.T) {
	req := CacheRequest{
		Op:     OpPrepare,
		Export: ExportCWD,
		Port:   30001,
		Cache:  VolumeNameForID("aabbccdd", "cwd"),
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}
