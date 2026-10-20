package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// maxPortViewBody bounds how much of a container list or inspect is read to
// rewrite its ports. moby sends JSON without a Content-Length, so anything
// over about 2 KiB arrives chunked; a list of a few thousand containers is
// still well under this, and a larger body is relayed as it came.
const maxPortViewBody = 16 << 20

// PortView rewrites the port fields of the two responses that show them, to
// the numbers opened on this machine (ADR 0008). Each returns false to leave
// the daemon's body alone.
type PortView interface {
	ContainerList(body []byte) ([]byte, bool)
	ContainerInspect(body []byte) ([]byte, bool)
}

type portViewKind int

const (
	notPortView portViewKind = iota
	containerList
	containerInspect
)

// portViewOf matches GET /containers/json and GET /containers/{id}/json,
// versioned or not, and nothing under or beside them (ADR 0005).
func portViewOf(req *http.Request) portViewKind {
	if req.Method != http.MethodGet {
		return notPortView
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(parts[0]) > 1 && parts[0][0] == 'v' && parts[0][1] >= '0' && parts[0][1] <= '9' {
		parts = parts[1:]
	}
	switch {
	case len(parts) == 2 && parts[0] == "containers" && parts[1] == "json":
		return containerList
	case len(parts) == 3 && parts[0] == "containers" && parts[1] != "" && parts[2] == "json":
		return containerInspect
	}
	return notPortView
}

// viewPorts replaces resp's body with the rewritten one when it is a complete
// JSON 200 under maxPortViewBody that PortView changed. Every other outcome
// leaves resp relaying the daemon's bytes. A chunked body is fine: ReadResponse
// has already removed the framing, and the new one is sent with its length.
func (p *Proxy) viewPorts(req *http.Request, resp *http.Response) {
	kind := portViewOf(req)
	if kind == notPortView || resp.StatusCode != http.StatusOK || contentType(resp) != "application/json" {
		return
	}

	head, err := io.ReadAll(io.LimitReader(resp.Body, maxPortViewBody+1))
	if err != nil || len(head) > maxPortViewBody {
		resp.Body = readCloser{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
		return
	}
	resp.Body = readCloser{bytes.NewReader(head), resp.Body}

	var rewritten []byte
	var ok bool
	if kind == containerList {
		rewritten, ok = p.PortView.ContainerList(head)
	} else {
		rewritten, ok = p.PortView.ContainerInspect(head)
	}
	if !ok {
		return
	}
	resp.Body = readCloser{bytes.NewReader(rewritten), resp.Body}
	resp.ContentLength = int64(len(rewritten))
	resp.TransferEncoding = nil
	resp.Header.Del("Content-Length")
	resp.Header.Del("Transfer-Encoding")
}
