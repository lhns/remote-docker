// Package cache is the contract for the delegated-share cache channel: its
// requests and replies, the codecs a payload may use, and the tar it carries
// (ADR 0044).
//
// A delegated share is a union: the live NFS export underneath and a local
// cache on top, so a read the cache lacks falls through and is still correct.
// Separate from core/notify, which carries no content and mutates nothing.
// Every op goes THROUGH the merged mount; agent/internal/unions/write.go says
// why. The policy is the dircache module, the union core-agent/union.
package cache

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// Command carries a delegated share's cache. An agent too old to know it runs
// `sh -c "workspace-cache"` and exits 127, so the client refuses the mode
// naming the workspace rather than failing half way through a mount.
const Command = "workspace-cache"

const (
	// Version is announced by the agent first, so a mismatch is a refusal
	// rather than a stall.
	Version = 1

	// MaxFrame bounds one JSON header line, not the payload after it.
	MaxFrame = 1 << 20

	// CodecNone is a plain tar, which every version reads.
	CodecNone = ""

	// CodecZstd is a zstd stream wrapping the tar. It cost the agent a direct
	// dependency (ADR 0021), paid because the fill is this protocol's one bulk
	// transfer.
	CodecZstd = "zstd"
)

// Codecs are what this version can read, announced in the greeting so a client
// never sends one the agent would refuse.
func Codecs() []string { return []string{CodecZstd} }

func supportsCodec(codec string) bool { return Hello{Codecs: Codecs()}.Accepts(codec) }

// Op is what one frame asks for.
type Op string

const (
	// OpPrepare mounts a share's union and answers where it landed.
	// Idempotent.
	OpPrepare Op = "prepare"

	// OpApply writes a tar into the union, for the fill and for a change made
	// on the client alike.
	OpApply Op = "apply"

	// OpDrop removes paths from the union. The Docker API can write into a
	// volume and never remove from one, which is why the agent does it.
	OpDrop Op = "drop"

	// OpChanges asks what the container wrote to the cache layer. The fill
	// writes through the union too, and the manifest separates the two.
	OpChanges Op = "changes"

	// OpPull asks for the bytes of named paths out of the cache layer.
	OpPull Op = "pull"

	// OpPresent asks which of the named paths exist in the MERGED view of the
	// share: what a container sees, the cache over the live export. Answered
	// with lstat, never following a link. Under Hello.Carry. The client asks it
	// before deleting anything a directory's removal or replacement implies, and
	// keeps whatever is present or cannot be answered.
	OpPresent Op = "present"

	// OpMounted asks which cache volumes this account has a union on. A union
	// is bound by PATH, so the daemon calls its volume unused and only the
	// workspace can answer for the collector (ADR 0044).
	OpMounted Op = "mounted"
)

// Change is one thing the container did to a share.
type Change struct {
	// Path is within the share, leading slash, forward slashes.
	Path string `json:"p"`

	Size int64 `json:"s,omitempty"`

	// ModTime is Unix nanoseconds on the WORKSPACE's clock, compared with the
	// client's only for a file both changed, after the measured offset.
	ModTime int64 `json:"m,omitempty"`

	// Deleted is a whiteout in the upper layer, which is how a deletion is told
	// from a file never cached. A whiteout of a directory is one Change with
	// the directory's path: what lay under it is the client's to work out.
	Deleted bool `json:"d,omitempty"`

	// Opaque says the path is a directory the container replaced: what the
	// lower held under it is hidden, and only what is listed beside this
	// change exists there now. Sent only to a client that asked (Request.Carry).
	Opaque bool `json:"o,omitempty"`

	// Link is the target of a symbolic link the container made at Path, kept
	// verbatim, however absolute or far outside the share. A link is a name, not
	// a write through it. Sent only to a client that asked (Request.Carry); one
	// that did not is never told of a link, because it would read it as an empty
	// file.
	Link string `json:"l,omitempty"`
}

// Request is one frame from the client: one op and the fields it needs.
type Request struct {
	Op Op `json:"op"`

	// Export is "/m/<id>", as the NFS export and volumes name it.
	Export string `json:"e"`

	// Port is the client's reverse-tunnel port, for OpPrepare. The agent
	// mounts the lower itself and records the port nowhere, so a share
	// prepared after a reconnect gets the new one: the one escape from ADR
	// 0032's "a volume names the port it was built for, forever".
	Port int `json:"p,omitempty"`

	// Cache is the managed volume holding the cache layer, for OpPrepare. On
	// the daemon's data root because the kernel refuses a union layer on
	// overlayfs, which a dind's own root is.
	Cache string `json:"c,omitempty"`

	// Read is the share's read mode, for OpPrepare: the attribute cache on the
	// union's lower. Empty means cached, from clients before the field.
	Read string `json:"r,omitempty"`

	// Owner is the uid and gid the share reports, "uid:gid", for OpPrepare: the
	// merged root is the upper directory the agent creates as root, and a
	// container running as another uid is refused by git there (ADR 0046).
	// Empty means 0:0, which is also what an agent before the field does, so a
	// client sends it only when it is not that and the agent announced
	// Hello.Owner.
	Owner string `json:"o,omitempty"`

	// Paths are what OpDrop removes and OpPull fetches, spelled as a share
	// path: leading slash, forward slashes, no "." or "..".
	Paths []string `json:"d,omitempty"`

	// Bytes is the length of the tar after this frame, for OpApply: a tar is
	// binary, so it is framed by length rather than delimited.
	Bytes int64 `json:"n,omitempty"`

	// Codec is how that tar is encoded. Empty is uncompressed.
	Codec string `json:"z,omitempty"`

	// Carry asks for the write-back behaviour of Hello.Carry: on OpChanges,
	// opaque directories and links are reported (Change.Opaque, Change.Link);
	// on OpApply, an entry that cannot be written is skipped and named in
	// Reply.Skipped instead of failing the whole batch. A client sends it only
	// when the agent announced the capability, and an agent that has not got it
	// ignores the field.
	Carry bool `json:"y,omitempty"`
}

// Reply is the agent's answer to one request.
type Reply struct {
	// Err is empty on success, and otherwise what a person is shown.
	Err string `json:"err,omitempty"`

	// Merged answers OpPrepare: where the union is mounted, as a path in the
	// daemon's namespace (ADR 0041).
	Merged string `json:"m,omitempty"`

	// Changes answers OpChanges.
	Changes []Change `json:"c,omitempty"`

	// Caches answers OpMounted.
	Caches []string `json:"v,omitempty"`

	// Unknown says the workspace has no union for the share, as distinct from
	// the request failing. A released share would otherwise be asked about for
	// the life of the session.
	Unknown bool `json:"unknown,omitempty"`

	// Skipped answers an OpApply or an OpPull that asked for Carry: the paths of
	// entries that could not be written (or read) and were left out, where the
	// rest of the batch went. Spelled as a share path.
	Skipped []string `json:"k,omitempty"`

	// Present answers OpPresent: the subset of the paths that exist.
	Present []string `json:"x,omitempty"`

	// Bytes is the length of the tar after this reply, answering OpPull.
	Bytes int64 `json:"n,omitempty"`

	// Hello announces the version, on the first line and nothing else.
	Hello *Hello `json:"hello,omitempty"`

	// Payload is the tar Bytes announced, filled in by the reader.
	Payload []byte `json:"-"`
}

// Hello is the agent's opening line, sent before it reads anything.
type Hello struct {
	Version int `json:"v"`

	// Codecs are the encodings this agent reads. Absent means a plain tar only,
	// which is why the client picks from THIS list and never from what it can
	// produce.
	Codecs []string `json:"z,omitempty"`

	// Owner says this agent gives a union's root the owner a prepare names. An
	// agent without it ignores the field, so the root stays root's and a
	// non-root container cannot use the share: the client refuses that mount
	// by name instead (cache.Hello.Owner is how it knows).
	Owner bool `json:"o,omitempty"`

	// Carry says this agent reports opaque directories and links in OpChanges
	// and skips and reports an unwritable entry of an OpApply, each only when
	// the request carries Carry (ADR 0044). A client that sees it absent
	// carries nothing back that is not a plain file, and says so in its
	// status.
	Carry bool `json:"w,omitempty"`
}

// Accepts reports whether the agent that sent this greeting can read a codec.
func (h Hello) Accepts(codec string) bool {
	return codec == CodecNone || slices.Contains(h.Codecs, codec)
}

// OwnerOf is the owner a prepare asks for: 0:0 when it names none.
func (r Request) OwnerOf() (workspace.Owner, error) {
	if r.Owner == "" {
		return workspace.Owner{Set: true}, nil
	}
	return workspace.ParseOwner(r.Owner)
}

// Validate rejects a request the agent should not act on. Called on both
// sides: on the agent it is the only thing between a malformed path and a
// privileged syscall.
func (r Request) Validate() error {
	// OpMounted asks about every share, so it names no export.
	if r.Op == OpMounted {
		return nil
	}
	if err := workspace.ValidExport(r.Export); err != nil {
		return fmt.Errorf("workspace: cache request export: %w", err)
	}

	switch r.Op {
	case OpPrepare:
		if r.Port < 1 || r.Port > workspace.MaxPort {
			return fmt.Errorf("workspace: cache prepare for %s names port %d, which is not one",
				r.Export, r.Port)
		}
		if !workspace.IsManagedVolume(r.Cache) {
			return fmt.Errorf("workspace: cache prepare for %s names %q, which is not a managed volume",
				r.Export, r.Cache)
		}
		if _, err := r.OwnerOf(); err != nil {
			return fmt.Errorf("workspace: cache prepare for %s: %w", r.Export, err)
		}
	case OpApply:
		if r.Bytes < 0 {
			return fmt.Errorf("workspace: cache apply for %s has %d bytes", r.Export, r.Bytes)
		}
		if !supportsCodec(r.Codec) {
			return fmt.Errorf("workspace: cache apply for %s asks for codec %q, which this version does not have",
				r.Export, r.Codec)
		}
	case OpDrop:
		if len(r.Paths) == 0 {
			return fmt.Errorf("workspace: cache drop for %s names no paths", r.Export)
		}
		for _, p := range r.Paths {
			if err := workspace.ValidSharePath(p); err != nil {
				return fmt.Errorf("workspace: cache drop: %w", err)
			}
			if strings.TrimSpace(p) == "/" {
				// The share root is the mount itself.
				return fmt.Errorf("workspace: cache drop for %s names the share root", r.Export)
			}
		}
	case OpChanges:
	case OpPull, OpPresent:
		if len(r.Paths) == 0 {
			return fmt.Errorf("workspace: cache %s for %s names no paths", r.Op, r.Export)
		}
		for _, p := range r.Paths {
			if err := workspace.ValidSharePath(p); err != nil {
				return fmt.Errorf("workspace: cache pull: %w", err)
			}
		}
	default:
		return fmt.Errorf("workspace: cache request has unknown op %q", r.Op)
	}
	return nil
}
