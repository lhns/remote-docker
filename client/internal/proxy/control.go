package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// The control channel rides the Docker endpoint rather than a socket of its
// own.
//
// One endpoint means one lock, one ACL and one thing to find. A second socket
// would need all three again, and would need them to agree, and the Docker
// endpoint's permissions already say exactly who may drive this session,
// because anyone who can reach it can already start containers that read and
// write this machine's filesystem.
//
// The prefix cannot collide with the Docker API: every real path begins
// /vX.YZ/ or one of the documented roots, and no version of the Engine API has
// ever served anything under a leading underscore.
const ControlPrefix = "/_remote-docker/"

// Control answers the session's own endpoints. Nil disables them, which is
// what a session that is not the daemon wants.
type Control interface {
	// Status describes the session for `remote-docker status`.
	Status() any

	// Shutdown asks the session to stop. It must return promptly and do the
	// stopping in the background: the caller is still holding the connection
	// that the shutdown is about to close.
	Shutdown()

	// Idle reports whether the session could be ended without breaking
	// anything. Separate from Status because answering it costs a round trip
	// to the workspace, and Status is asked far more often than the answer is
	// needed.
	Idle() any
}

func isControl(req *http.Request) bool {
	return strings.HasPrefix(req.URL.Path, ControlPrefix)
}

// serveControl answers a control request. It never reaches the workspace.
func (p *Proxy) serveControl(client net.Conn, req *http.Request) {
	if p.Control == nil {
		writeControl(client, http.StatusNotFound,
			map[string]string{"message": "this endpoint is not served by a remote-docker daemon"})
		return
	}

	switch strings.TrimPrefix(req.URL.Path, ControlPrefix) {
	case "status":
		writeControl(client, http.StatusOK, p.Control.Status())

	case "idle":
		writeControl(client, http.StatusOK, p.Control.Idle())

	case "shutdown":
		if req.Method != http.MethodPost {
			writeControl(client, http.StatusMethodNotAllowed,
				map[string]string{"message": "shutdown must be POSTed"})
			return
		}
		// Answered before acting. Shutdown closes this very connection, so
		// replying afterwards would be replying into a socket we just closed
		// and the caller would see an unexplained EOF instead of an
		// acknowledgement.
		writeControl(client, http.StatusOK, map[string]string{"status": "stopping"})
		p.Control.Shutdown()

	default:
		writeControl(client, http.StatusNotFound,
			map[string]string{"message": "no such control endpoint"})
	}
}

func writeControl(w io.Writer, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = []byte(`{"message":"encoding the response failed"}`)
		status = http.StatusInternalServerError
	}
	// Connection: close, because a control call is a one-shot and leaving the
	// connection open would have the caller's transport hold it idle, which
	// for `stop` means holding the very session it just asked to end.
	_, _ = fmt.Fprintf(w,
		"HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(encoded), encoded)
}

// Status is what the daemon reports about itself.
type Status struct {
	// Version is the build the daemon is running.
	//
	// Compared, never ordered. A sha build says which commit and nothing about
	// when: "sha-a7634c0" and "sha-95e42ac" cannot be put in sequence, and a
	// release version cannot be compared with either. So a difference is
	// reported as a difference, never as "outdated": claiming an order this
	// cannot know would be worse than saying nothing.
	Version string `json:"version"`

	// Storage is the graph driver of the daemon this session is talking to.
	//
	// Carried here so an ordinary `docker` command can warn about it. The
	// session is the only thing that has spoken to the workspace, since every
	// other command talks to the session, so without this the fact would be
	// reachable only by running `status` on purpose, which is not something
	// somebody does while wondering why their container is slow.
	Storage string `json:"storage,omitempty"`

	// Tracing is whether the session was started with TraceEnv set.
	//
	// Here for the same reason Storage is: only the session can answer it, and
	// the question is asked by somebody standing at another command wondering
	// why setting the variable printed nothing.
	Tracing bool `json:"tracing,omitempty"`

	PID       int    `json:"pid"`
	Connected bool   `json:"connected"`
	Since     string `json:"since"`

	// Caches is one line per delegated share saying how much of it is cached
	// (ADR 0044).
	//
	// Reported because a partly cached share is not a failure and has nothing
	// else to show for itself: it works, it is simply slower for the part that
	// did not fit, and without this the only symptom is a directory that feels
	// fast in places. A share still filling reads the same way, which is the
	// point -- both are "some of it is local", and neither is wrong.
	Caches []string `json:"caches,omitempty"`

	// Drops is how many times this session has found its connection dead and
	// opened another, and LastDrop when it last did.
	//
	// Reported because reconnecting is invisible otherwise: a session that
	// does it once is working, one that does it every few minutes is a link
	// worth looking at, and neither can be told from the outside. Carried with
	// the time so "twice, an hour ago" reads differently from "twice, just
	// now"; a session that recovered is not a session with a problem.
	Drops    int    `json:"drops,omitempty"`
	LastDrop string `json:"lastDrop,omitempty"`
}

// Idle is what the daemon reports about whether it can be ended.
type Idle struct {
	// Safe means nothing depends on this session: no container of ours
	// running, no stream open, no shell. Restarting it would lose nothing.
	//
	// Never a guess. The check that produces it is the one guarding the idle
	// timer, and it answers "cannot tell" as busy.
	Safe bool `json:"safe"`

	// Quiet is how long the session has had nothing to do.
	Quiet string `json:"quiet"`
}
