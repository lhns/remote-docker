package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// ControlPrefix is the session's own API, served on the Docker endpoint so it
// shares that endpoint's lock and ACL. No Engine API path starts with an
// underscore.
const ControlPrefix = "/_remote-docker/"

// Control answers the session's own endpoints. Nil disables them.
type Control interface {
	Status() any

	// Shutdown must return promptly and stop in the background: the caller
	// still holds the connection the shutdown will close.
	Shutdown()

	// Idle is separate from Status because it costs a round trip to the
	// workspace.
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
		// Reply first: Shutdown closes this connection.
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
	// Connection: close, or the caller's transport holds it idle, which for
	// `stop` pins the session it asked to end.
	_, _ = fmt.Fprintf(w,
		"HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(encoded), encoded)
}

// Status is what the daemon reports about itself.
type Status struct {
	// Version is compared, never ordered: sha builds have no sequence, so a
	// difference is reported as a difference, never as "outdated".
	Version string `json:"version"`

	// Storage is the workspace daemon's graph driver; Tracing whether TraceEnv
	// was set. Carried here so an ordinary `docker` command can warn about them.
	Storage string `json:"storage,omitempty"`
	Tracing bool   `json:"tracing,omitempty"`

	PID       int    `json:"pid"`
	Connected bool   `json:"connected"`
	Since     string `json:"since"`

	// Caches is one line per delegated share saying how much is cached (ADR 0044).
	Caches []string `json:"caches,omitempty"`

	// Drops counts reconnects after a dead connection; otherwise invisible.
	Drops    int    `json:"drops,omitempty"`
	LastDrop string `json:"lastDrop,omitempty"`
}

// Idle is what the daemon reports about whether it can be ended.
type Idle struct {
	// Safe means nothing depends on this session: no container of ours
	// running, no stream, no shell. "Cannot tell" answers false.
	Safe bool `json:"safe"`
}
