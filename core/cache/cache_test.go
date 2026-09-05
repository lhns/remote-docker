package cache

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// The client and the agent both call Validate, and the agent's call is the one
// that matters: this stream tells a root process which paths to write and which
// to remove inside the workspace.
func TestCacheRequestValidate(t *testing.T) {
	const share = "/m/00112233445566ff"
	cache := workspace.VolumeNameForID("aabbccdd", "00112233445566ff")

	for _, c := range []struct {
		name string
		req  Request
		want string // a substring of the refusal, or "" to accept
	}{
		{
			name: "a prepare with a port and a managed volume",
			req:  Request{Op: OpPrepare, Export: share, Port: 30001, Cache: cache},
		},
		{
			name: "a prepare with no port has no address to mount from",
			req:  Request{Op: OpPrepare, Export: share, Cache: cache},
			want: "port",
		},
		{
			// The agent would otherwise mount whatever it was handed, inside
			// the account's daemon, as root.
			name: "a prepare naming a volume this client did not create",
			req:  Request{Op: OpPrepare, Export: share, Port: 30001, Cache: "postgres-data"},
			want: "managed volume",
		},
		{
			name: "an apply",
			req:  Request{Op: OpApply, Export: share, Bytes: 4096},
		},
		{
			name: "an apply of nothing, which is how an empty batch looks",
			req:  Request{Op: OpApply, Export: share},
		},
		{
			// The field is what makes compression a negotiation rather than a
			// new protocol. Saying yes to one this version has not got would
			// be worse than refusing: the payload would reach the archive
			// reader as whatever the bytes happened to be.
			name: "an apply asking for a codec this version has not got",
			req:  Request{Op: OpApply, Export: share, Bytes: 10, Codec: "brotli"},
			want: "codec",
		},
		{
			name: "an apply compressed with the codec this version has",
			req:  Request{Op: OpApply, Export: share, Bytes: 10, Codec: CodecZstd},
		},
		{
			name: "a drop",
			req:  Request{Op: OpDrop, Export: share, Paths: []string{"/pkg/lib.go"}},
		},
		{
			name: "a drop that names nothing",
			req:  Request{Op: OpDrop, Export: share},
			want: "no paths",
		},
		{
			name: "a drop that walks out of the share",
			req:  Request{Op: OpDrop, Export: share, Paths: []string{"/../../etc/shadow"}},
			want: "component",
		},
		{
			// Removing the share root is unmounting it, and the mount goes
			// when the channel does rather than on request.
			name: "a drop that names the share root",
			req:  Request{Op: OpDrop, Export: share, Paths: []string{"/"}},
			want: "root",
		},
		{
			name: "an export that is not one this program serves",
			req:  Request{Op: OpChanges, Export: "/etc"},
			want: "export",
		},
		{
			name: "an op from a client newer than this agent",
			req:  Request{Op: "promote", Export: share},
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
	req := Request{
		Op:     OpPrepare,
		Export: workspace.ExportCWD,
		Port:   30001,
		Cache:  workspace.VolumeNameForID("aabbccdd", "cwd"),
	}
	if err := req.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

// Compression is a negotiation rather than a new format, which is what the
// codec field has been there for since version 1. The case that matters is an
// agent OLDER than it: its greeting names no codecs, and a client must then
// send a plain tar rather than something it would refuse.
func TestCacheCodecNegotiation(t *testing.T) {
	for _, c := range []struct {
		name  string
		hello Hello
		codec string
		want  bool
	}{
		{
			name:  "an agent that announces zstd",
			hello: Hello{Version: Version, Codecs: Codecs()},
			codec: CodecZstd,
			want:  true,
		},
		{
			// The whole reason the client picks from the greeting rather than
			// from what it can produce.
			name:  "an agent from before compression",
			hello: Hello{Version: Version},
			codec: CodecZstd,
			want:  false,
		},
		{
			name:  "a plain tar, which every version reads",
			hello: Hello{Version: Version},
			codec: CodecNone,
			want:  true,
		},
		{
			name:  "a codec nobody has",
			hello: Hello{Version: Version, Codecs: Codecs()},
			codec: "brotli",
			want:  false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := c.hello.Accepts(c.codec); got != c.want {
				t.Errorf("Accepts(%q) = %v, want %v", c.codec, got, c.want)
			}
		})
	}
}

// And the agent's side of the same rule: a batch naming an encoding this
// version has not got is refused by name, before anything reads it as a tar.
func TestCacheRequestCodecs(t *testing.T) {
	const share = "/m/00112233445566ff"

	if err := (Request{Op: OpApply, Export: share, Bytes: 10, Codec: CodecZstd}).Validate(); err != nil {
		t.Errorf("a zstd batch was refused: %v", err)
	}
	if err := (Request{Op: OpApply, Export: share, Bytes: 10, Codec: "brotli"}).Validate(); err == nil {
		t.Error("a batch named an encoding this version has not got and was accepted")
	}
	if !supportsCodec(CodecNone) {
		t.Error("a plain tar was not supported")
	}
}
