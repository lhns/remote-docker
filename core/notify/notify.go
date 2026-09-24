// Package notify is the contract for the change-notification channel: NFS
// carries no change notification, so the client watches its own filesystem and
// tells the agent which paths to touch, and the kernel emits the event a
// container's watcher waits for (ADR 0014, ADR 0016).
package notify

import (
	"fmt"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// Command carries the client's changes to be replayed in the workspace. An
// agent too old to know it runs `sh -c "workspace-notify"` and exits 127, which
// is the version check.
const Command = "workspace-notify"

const (
	// Version is announced by the agent first, so a working channel can be
	// told from that exit 127.
	Version = 1

	// MaxFrame bounds one line. Both ends must agree: a scanner smaller than
	// the writer's frame silently truncates the stream at the first large
	// batch.
	MaxFrame = 1 << 20
)

// Op is what happened to a path. One event may carry several: coalescing an
// editor's save-in-place yields OpCreate|OpWrite.
type Op uint8

const (
	OpCreate Op = 1 << iota
	OpWrite
	OpRemove
	OpRename
	OpAttrib

	// opAll is every defined bit, so Validate can reject the rest rather than
	// letting an unknown op reach the agent's replay switch.
	opAll = OpCreate | OpWrite | OpRemove | OpRename | OpAttrib
)

func (o Op) String() string {
	if o == 0 {
		return "none"
	}
	var names []string
	for _, b := range []struct {
		op   Op
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

// Event is one change to one path, as the client observed it. It carries no
// content and never will: the bytes are already there through NFS, and only
// the notification is missing (ADR 0014).
type Event struct {
	// Export is the share the path belongs to: "/cwd" or "/m/<id>".
	Export string `json:"e"`

	// Path is within the share, always leading-slash and always "/"-separated
	// however the client's OS spells it. The share root itself is "/".
	Path string `json:"p"`

	// Op is the merged operation set. Zero is invalid.
	Op Op `json:"o"`

	// Dir says the path is, or was, a directory: the replay differs, and after
	// a removal the agent cannot look.
	Dir bool `json:"d,omitempty"`
}

// Validate rejects anything that is not a well-formed in-share path. Called on
// both sides: on the agent it is the only thing between a malformed path and a
// privileged syscall.
func (e Event) Validate() error {
	if err := workspace.ValidExport(e.Export); err != nil {
		return fmt.Errorf("workspace: notify event export: %w", err)
	}
	if err := workspace.ValidSharePath(e.Path); err != nil {
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

// Notice tells the receiver that the client's view is incomplete under Path.
// Never omitted when something was lost: a receiver that believes it saw
// everything is the failure this channel exists to remove.
type Notice struct {
	Export string `json:"e"`

	// Path is the deepest directory covering everything that was lost.
	Path string `json:"p"`

	// Dropped is how many events were lost, or 0 when that is not known.
	Dropped int `json:"n,omitempty"`

	// Reason is why: overflow, budget, queue, rate or disconnected.
	Reason string `json:"r"`
}

// Hello is the agent's opening line, sent before anything else.
type Hello struct {
	Version int    `json:"v"`
	Agent   string `json:"a,omitempty"`
}

// Frame is one line of the stream. Exactly one payload field is set.
//
// Events are batched per frame rather than sent one per line: the flush
// boundary is itself information the agent can dedupe within, and a save that
// touches twelve files should not cost twelve round trips through the SSH
// window.
type Frame struct {
	Hello  *Hello  `json:"h,omitempty"`
	Events []Event `json:"v,omitempty"`
	Notice *Notice `json:"x,omitempty"`
}
