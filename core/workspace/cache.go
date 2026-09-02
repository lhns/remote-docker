package workspace

import (
	"fmt"
	"strings"
)

// The cache channel: what a delegated share's union mount is told (ADR 0044).
//
// A delegated share is not a copy of the client's tree but a UNION of two
// layers the workspace mounts: the live NFS export underneath, and a local
// cache on top. A read the cache has is local disk; a read it does not have
// falls through and is correct. So the cache is allowed to be incomplete at
// every moment, which is what lets it be filled in the background.
//
// This channel is deliberately not core/workspace/notify.go, whose contract
// says it carries no content and never will. That one makes a watcher fire and
// mutates nothing. This one ships bytes and writes them, which is a different
// promise, and separating the two is what keeps the first one true.
//
// Every op here is performed THROUGH the merged mount rather than into the
// cache layer directly, which is a kernel constraint. The reasoning is in
// agent/internal/unions/write.go, beside the code it binds.
// CacheCommand carries a delegated share's cache: preparing its union mount,
// filling it, and invalidating what changed on the client (ADR 0044).
//
// Deliberately not NotifyCommand, which promises to carry no content and to
// mutate nothing. This one is a sync, and a channel of its own is what keeps
// that promise true of the other.
//
// The same version check: an agent too old to know it runs
// `sh -c "workspace-cache"` and exits 127, so the client refuses the mode
// naming the workspace rather than discovering it half way through a mount.
const CacheCommand = "workspace-cache"

const (
	// CacheVersion is the wire version, announced by the agent before anything
	// else so a mismatch is a refusal rather than a stall.
	CacheVersion = 1

	// MaxCacheFrame bounds one JSON header line. The payload that follows a
	// frame is not a line and is not bounded by this; only the header is.
	MaxCacheFrame = 1 << 20

	// CodecNone is an uncompressed payload, which is what an empty codec means
	// and what every version can read.
	CodecNone = ""

	// CodecZstd is a zstd stream wrapping the tar.
	//
	// It costs the agent a direct dependency, which is not free on that side:
	// ADR 0021 split the modules so the agent's graph could be small and
	// stated, and this takes it from four direct requires to five. Paid
	// deliberately, because the fill is the one bulk transfer this protocol
	// makes and zstd compresses a source tree harder and faster than the
	// standard library's gzip does.
	CodecZstd = "zstd"
)

// Codecs are what this version can read, announced in the greeting so a client
// never sends one the agent would refuse.
func Codecs() []string { return []string{CodecZstd} }

// SupportsCodec reports whether a codec is one this version can read. The empty
// codec is always readable: it is a plain tar.
func SupportsCodec(codec string) bool {
	if codec == CodecNone {
		return true
	}
	for _, c := range Codecs() {
		if c == codec {
			return true
		}
	}
	return false
}

// CacheOp is what one frame asks for.
type CacheOp string

const (
	// OpPrepare mounts a share's union and answers with where it landed. Sent
	// before the container that needs it is created, and idempotent: a share
	// already prepared answers with the same path.
	OpPrepare CacheOp = "prepare"

	// OpApply writes a tar into the union. Both the fill and the way a change
	// on the client reaches the cache: there is no difference between the two.
	OpApply CacheOp = "apply"

	// OpDrop removes paths from the union: what a deletion on the client
	// becomes. The Docker API can write into a volume and never remove from
	// one, which is why the agent is involved at all.
	OpDrop CacheOp = "drop"

	// OpChanges asks what the cache layer holds that the client did not put
	// there, which is what the container wrote. The layer alone cannot say:
	// the fill writes through the union too, and the manifest is what
	// separates them (ADR 0044).
	OpChanges CacheOp = "changes"

	// OpPull asks for the bytes of named paths out of the cache layer, so the
	// client can write them back to its own disk.
	OpPull CacheOp = "pull"

	// OpMounted asks which cache volumes this account has a union on, for the
	// volume collector: a union is bound into a container by PATH, so nothing
	// references the volume and the daemon calls it unused (ADR 0044). Names
	// no export, because the question is about all of them at once.
	OpMounted CacheOp = "mounted"
)

// CacheChange is one thing the container did to a share.
type CacheChange struct {
	// Path is within the share, leading slash, forward slashes.
	Path string `json:"p"`

	Size int64 `json:"s,omitempty"`

	// ModTime is when the container wrote it, in Unix nanoseconds on the
	// WORKSPACE's clock. Compared against the client's own only for a file
	// both sides changed, and only after the session's measured offset is
	// applied -- two clocks that were never set together.
	ModTime int64 `json:"m,omitempty"`

	// Deleted says the container removed it. An overlay records that as a
	// whiteout in the upper layer, which is why a deletion can be told from a
	// file that was simply never cached.
	Deleted bool `json:"d,omitempty"`
}

// CacheRequest is one frame from the client. Exactly one op, and the fields
// that op needs.
type CacheRequest struct {
	Op CacheOp `json:"op"`

	// Export is the share this concerns: "/cwd" or "/m/<id>", the same names
	// the NFS export and the volumes use.
	Export string `json:"e"`

	// Port is the client's reverse-tunnel port, for OpPrepare. The agent
	// mounts the lower itself rather than leaving it to a volume, so it needs
	// the address; and because nothing durable records it, a share prepared
	// again after a reconnect simply gets the new one. That is the one way
	// this design escapes ADR 0032's "a volume names the port it was built
	// for, forever".
	Port int `json:"p,omitempty"`

	// Cache is the volume whose data directory holds the cache layer, for
	// OpPrepare. A managed volume rather than a directory of the agent's, so
	// naming (ADR 0029), labelling and collection are unchanged -- and because
	// its data lives on the daemon's data root, which is a real filesystem.
	// The kernel refuses a union layer on overlayfs, and a dind's own root is
	// overlayfs.
	Cache string `json:"c,omitempty"`

	// Paths are what OpDrop removes, each within the share and spelled the way
	// FSEvent spells one: leading slash, forward slashes, no "." or "..".
	Paths []string `json:"d,omitempty"`

	// Bytes is the length of the tar that follows this frame, for OpApply.
	// Length-prefixed rather than delimited because a tar is binary and any
	// delimiter would have to be escaped out of it.
	Bytes int64 `json:"n,omitempty"`

	// Codec names how that tar is encoded. Empty is uncompressed; the field
	// exists from version 1 so turning compression on later is a negotiation
	// rather than a new protocol.
	Codec string `json:"z,omitempty"`
}

// CacheReply is the agent's answer to one request.
type CacheReply struct {
	// Err is empty on success. A refusal names what it refused and why, since
	// this is the only thing the client can show a person.
	Err string `json:"err,omitempty"`

	// Merged is where the union is mounted, answering OpPrepare. It is a path
	// inside the daemon's namespace, which is what the container binds -- the
	// same shape as the paths a workspace already declares (ADR 0041).
	Merged string `json:"m,omitempty"`

	// Changes answers OpChanges: what the container did to the share.
	Changes []CacheChange `json:"c,omitempty"`

	// Caches answers OpMounted: the cache volumes this account has a union on.
	Caches []string `json:"v,omitempty"`

	// Unknown says the workspace has no union for the share the request named,
	// which is a different thing from the request failing. A share is released
	// when nothing holds it, and a client that cannot tell the two apart goes
	// on asking about it for the life of the session.
	Unknown bool `json:"unknown,omitempty"`

	// Bytes is the length of the tar that follows this reply, answering
	// OpPull. Framed by length for the same reason a request's payload is: a
	// tar is binary, and any delimiter would have to be escaped out of it.
	Bytes int64 `json:"n,omitempty"`

	// Hello announces the version, on the first line and nothing else.
	Hello *CacheHello `json:"hello,omitempty"`

	// Payload is the tar that followed this reply, filled in by the reader
	// rather than by the wire: Bytes is what is sent, and this is what those
	// bytes turned out to be.
	Payload []byte `json:"-"`
}

// CacheHello is the agent's opening line, sent before it reads anything.
type CacheHello struct {
	Version int `json:"v"`

	// Codecs are the payload encodings this agent can read. Absent means it
	// predates compression and can read only a plain tar, which is why a
	// client picks from THIS list rather than from what it can produce: an
	// older workspace must never be sent something it would refuse.
	Codecs []string `json:"z,omitempty"`
}

// Accepts reports whether the agent that sent this greeting can read a codec.
func (h CacheHello) Accepts(codec string) bool {
	if codec == CodecNone {
		return true
	}
	for _, c := range h.Codecs {
		if c == codec {
			return true
		}
	}
	return false
}

// Validate rejects a request the agent should not act on.
//
// Called on BOTH sides, like FSEvent.Validate and for the same reason: this
// stream tells a root process which paths to write and which to remove inside
// the workspace. On the client a failure is our own bug; on the agent it is
// the only thing between a malformed path and a privileged syscall.
func (r CacheRequest) Validate() error {
	// Before the export check, because this one asks about every share at once
	// and so names none.
	if r.Op == OpMounted {
		return nil
	}
	if err := ValidExport(r.Export); err != nil {
		return fmt.Errorf("workspace: cache request export: %w", err)
	}

	switch r.Op {
	case OpPrepare:
		if r.Port < 1 || r.Port > MaxPort {
			return fmt.Errorf("workspace: cache prepare for %s names port %d, which is not one",
				r.Export, r.Port)
		}
		if !IsManagedVolume(r.Cache) {
			return fmt.Errorf("workspace: cache prepare for %s names %q, which is not a managed volume",
				r.Export, r.Cache)
		}
	case OpApply:
		if r.Bytes < 0 {
			return fmt.Errorf("workspace: cache apply for %s has %d bytes", r.Export, r.Bytes)
		}
		if !SupportsCodec(r.Codec) {
			return fmt.Errorf("workspace: cache apply for %s asks for codec %q, which this version does not have",
				r.Export, r.Codec)
		}
	case OpDrop:
		if len(r.Paths) == 0 {
			return fmt.Errorf("workspace: cache drop for %s names no paths", r.Export)
		}
		for _, p := range r.Paths {
			if err := ValidSharePath(p); err != nil {
				return fmt.Errorf("workspace: cache drop: %w", err)
			}
			if strings.TrimSpace(p) == "/" {
				// The share root is not a path the client may remove: it is
				// the mount itself, and the mount goes when the channel does.
				return fmt.Errorf("workspace: cache drop for %s names the share root", r.Export)
			}
		}
	case OpChanges:
	case OpPull:
		if len(r.Paths) == 0 {
			return fmt.Errorf("workspace: cache pull for %s names no paths", r.Export)
		}
		for _, p := range r.Paths {
			if err := ValidSharePath(p); err != nil {
				return fmt.Errorf("workspace: cache pull: %w", err)
			}
		}
	default:
		return fmt.Errorf("workspace: cache request has unknown op %q", r.Op)
	}
	return nil
}
