package workspace

import (
	"fmt"
	"strings"
)

// The change-notification channel.
//
// NFS carries no change notification, so a watcher inside a container sees
// nothing at all when the user edits a file on their own machine -- measured,
// not assumed (ADR 0014). Every hot-reload workflow depends on inotify and so
// does nothing while appearing to work.
//
// Linux offers no way to inject a synthetic event; fanotify(7) states plainly
// that it "does not catch remote events that occur on network filesystems".
// The only mechanism available to anyone is to perform a real VFS operation
// and let the kernel emit the event as a side effect. So the client watches
// its own filesystem, where the changes actually happen, and tells the agent
// which paths to touch.
const (
	// NotifyCommand is the long-lived channel carrying this client's
	// filesystem changes to the agent. Dispatched on the exact string, like
	// the other protocol commands.
	NotifyCommand = "workspace-notify"

	// NotifyVersion is the wire version. The agent announces it first: an
	// agent too old to know this command would otherwise fall through to the
	// generic exec path, run `sh -c "workspace-notify"` and exit 127, which
	// the client must be able to tell apart from a working channel.
	NotifyVersion = 1

	// MaxNotifyFrame bounds one line. Both ends must agree, because a scanner
	// with a smaller buffer than the writer's frame silently truncates the
	// stream at the first large batch.
	MaxNotifyFrame = 1 << 20
)

// FSOp is what happened to a path. One event may carry several: coalescing an
// editor's save-in-place yields OpCreate|OpWrite.
type FSOp uint8

const (
	OpCreate FSOp = 1 << iota
	OpWrite
	OpRemove
	OpRename
	OpAttrib

	// opAll is every defined bit, so Validate can reject the rest rather than
	// letting an unknown op reach the agent's replay switch.
	opAll = OpCreate | OpWrite | OpRemove | OpRename | OpAttrib
)

func (o FSOp) String() string {
	if o == 0 {
		return "none"
	}
	var names []string
	for _, b := range []struct {
		op   FSOp
		name string
	}{
		{OpCreate, "create"},
		{OpWrite, "write"},
		{OpRemove, "remove"},
		{OpRename, "rename"},
		{OpAttrib, "attrib"},
	} {
		if o&b.op != 0 {
			names = append(names, b.name)
		}
	}
	if rest := o &^ opAll; rest != 0 {
		names = append(names, fmt.Sprintf("unknown(%#x)", uint8(rest)))
	}
	return strings.Join(names, "|")
}

// FSEvent is one change to one path, as the client observed it.
//
// It carries no content and never will. The bytes are already in the container
// through the NFS mount; what is missing is only the kernel's notification.
// Shipping data here would turn this into a sync -- the thing ADR 0014 is
// trying not to become.
type FSEvent struct {
	// Export is the share the path belongs to: "/cwd" or "/m/<id>".
	Export string `json:"e"`

	// Path is within the share, always leading-slash and always "/"-separated
	// however the client's OS spells it. The share root itself is "/".
	Path string `json:"p"`

	// Op is the merged operation set. Zero is invalid.
	Op FSOp `json:"o"`

	// Dir says the path is, or was, a directory. The agent needs it because
	// the operation that makes a watcher notice a directory is not the one
	// that makes it notice a file, and after a removal it cannot go and
	// look.
	Dir bool `json:"d,omitempty"`
}

// Validate rejects anything that is not a well-formed in-share path.
//
// Called on BOTH sides, and that is the point: this stream tells a root
// process which path to touch inside the workspace. On the client a failure is
// a bug in our own watcher; on the agent it is the only thing between a
// malformed path and a privileged syscall. Neither end may assume the other
// checked.
func (e FSEvent) Validate() error {
	// VolumeNameForExport accepts exactly /cwd and /m/<16 hex> and rejects
	// everything else, which is the same set the agent can resolve to a
	// volume. Reusing it means the two cannot drift apart.
	if _, err := VolumeNameForExport(e.Export); err != nil {
		return fmt.Errorf("workspace: notify event export: %w", err)
	}
	if err := validateSharePath(e.Path); err != nil {
		return err
	}
	if e.Op == 0 {
		return fmt.Errorf("workspace: notify event for %q has no operation", e.Path)
	}
	if rest := e.Op &^ opAll; rest != 0 {
		return fmt.Errorf("workspace: notify event for %q has unknown operation bits %#x", e.Path, uint8(rest))
	}
	return nil
}

// validateSharePath enforces the wire spelling of an in-share path.
//
// Deliberately a whitelist rather than a blacklist, and deliberately not
// path.Clean: cleaning would *repair* a traversal into something plausible
// instead of refusing it, so "/a/../../etc/shadow" would arrive as a path the
// agent happily touches. A malformed path is a bug worth reporting, never one
// worth guessing about.
func validateSharePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("workspace: notify path is empty")
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("workspace: notify path %q is not absolute within its share", p)
	case strings.Contains(p, `\`):
		// A backslash is a path separator on the client and an ordinary
		// filename character in the workspace, so letting one through would
		// create a file whose name contains the rest of the path.
		return fmt.Errorf("workspace: notify path %q contains a backslash; separators must be normalised", p)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("workspace: notify path %q contains a NUL", p)
	}
	if p == "/" {
		return nil
	}
	for part := range strings.SplitSeq(strings.TrimPrefix(p, "/"), "/") {
		switch part {
		case "":
			return fmt.Errorf("workspace: notify path %q has an empty component", p)
		case ".", "..":
			return fmt.Errorf("workspace: notify path %q has a %q component", p, part)
		}
	}
	return nil
}

// FSNotice tells the receiver that the client's view is incomplete under Path,
// so it should do something coarser than replaying events, or nothing, if
// the tool being served rescans anyway.
//
// Never omitted when something was lost. A receiver that silently believes it
// has seen everything is precisely the failure this whole channel exists to
// remove, and reintroducing it one layer down would be worse than not having
// the channel: the user would have been told it works.
type FSNotice struct {
	Export string `json:"e"`

	// Path is the deepest directory covering everything that was lost.
	Path string `json:"p"`

	// Dropped is how many events were lost, or 0 when that is not known.
	Dropped int `json:"n,omitempty"`

	// Reason is why: overflow, budget, queue, rate or disconnected.
	Reason string `json:"r"`
}

// NotifyHello is the agent's opening line, sent before anything else.
type NotifyHello struct {
	Version int    `json:"v"`
	Agent   string `json:"a,omitempty"`
}

// NotifyFrame is one line of the stream. Exactly one payload field is set.
//
// Events are batched per frame rather than sent one per line: the flush
// boundary is itself information the agent can dedupe within, and a save that
// touches twelve files should not cost twelve round trips through the SSH
// window.
type NotifyFrame struct {
	Hello  *NotifyHello `json:"h,omitempty"`
	Events []FSEvent    `json:"v,omitempty"`
	Notice *FSNotice    `json:"x,omitempty"`
}
