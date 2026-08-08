package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// The control channel rides the Docker endpoint rather than a socket of its
// own.
//
// One endpoint means one lock, one ACL and one thing to find. A second socket
// would need all three again, and would need them to agree -- and the Docker
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

func writeControl(w net.Conn, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		encoded = []byte(`{"message":"encoding the response failed"}`)
		status = http.StatusInternalServerError
	}
	// Connection: close, because a control call is a one-shot and leaving the
	// connection open would have the caller's transport hold it idle -- which
	// for `stop` means holding the very session it just asked to end.
	_, _ = fmt.Fprintf(w,
		"HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(encoded), encoded)
}

// Status is what the daemon reports about itself.
type Status struct {
	Workspace string   `json:"workspace"`
	Host      string   `json:"host"`
	User      string   `json:"user"`
	Endpoint  string   `json:"endpoint"`
	PID       int      `json:"pid"`
	Connected bool     `json:"connected"`
	Ports     []int    `json:"ports,omitempty"`
	Since     string   `json:"since"`
	Watching  string   `json:"watching,omitempty"`
	Shares    []string `json:"shares,omitempty"`
}
