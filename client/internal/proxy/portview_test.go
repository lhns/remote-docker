package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestPortViewMatchesOnlyListAndInspect(t *testing.T) {
	tests := []struct {
		method, path string
		want         portViewKind
	}{
		{"GET", "/containers/json", containerList},
		{"GET", "/v1.51/containers/json", containerList},
		{"GET", "/containers/abc/json", containerInspect},
		{"GET", "/v1.51/containers/abc/json", containerInspect},
		{"POST", "/containers/json", notPortView},
		{"HEAD", "/containers/abc/json", notPortView},
		{"GET", "/containers/abc/json/extra", notPortView},
		{"GET", "/containers/abc/stats", notPortView},
		{"GET", "/containers/abc/top", notPortView},
		{"GET", "/containers//json", notPortView},
		{"GET", "/events", notPortView},
		{"GET", "/images/json", notPortView},
		{"GET", "/volumes/json", notPortView},
		{"GET", "/v1.51/networks/abc/json", notPortView},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, "http://docker"+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := portViewOf(req); got != tt.want {
				t.Errorf("portViewOf = %d, want %d", got, tt.want)
			}
		})
	}
}

// markView proves which hook ran by appending to the body, and keeps what it
// was given so a test can see the body arrived dechunked.
type markView struct {
	seen []string
	skip bool
}

func (m *markView) ContainerList(body []byte) ([]byte, bool) {
	m.seen = append(m.seen, "list:"+string(body))
	return append(append([]byte(nil), body...), []byte(`"L"`)...), !m.skip
}

func (m *markView) ContainerInspect(body []byte) ([]byte, bool) {
	m.seen = append(m.seen, "inspect:"+string(body))
	return append(append([]byte(nil), body...), []byte(`"I"`)...), !m.skip
}

// get sends GET through a proxy fronting a daemon that answers with raw, and
// returns the relayed response and body.
func get(t *testing.T, p *Proxy, path, raw string) (*http.Response, string) {
	t.Helper()
	d := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		io.WriteString(conn, raw)
	})
	p.Dialer = &tcpDialer{d.listener.Addr().String()}
	addr := startProxy(t, p)

	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func fixed(body string) string {
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

// chunked splits the body in two, the way moby's encoder writes a big one.
func chunked(body string) string {
	half := len(body) / 2
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n%x\r\n%s\r\n0\r\n\r\n",
		half, body[:half], len(body)-half, body[half:])
}

func TestProxyRewritesPortsOfAListAndAnInspect(t *testing.T) {
	for name, wrap := range map[string]func(string) string{"content-length": fixed, "chunked": chunked} {
		t.Run(name, func(t *testing.T) {
			view := &markView{}
			resp, body := get(t, &Proxy{PortView: view}, "/v1.51/containers/json", wrap(`["a"]`))
			if body != `["a"]"L"` {
				t.Errorf("list body = %q, want the hook's output", body)
			}
			if resp.ContentLength != int64(len(body)) || len(resp.TransferEncoding) != 0 {
				t.Errorf("framing: ContentLength %d, TransferEncoding %v, for %d bytes", resp.ContentLength, resp.TransferEncoding, len(body))
			}
			if len(view.seen) != 1 || view.seen[0] != `list:["a"]` {
				t.Errorf("hook saw %q", view.seen)
			}

			_, body = get(t, &Proxy{PortView: view}, "/containers/abc/json", wrap(`{"Id":"abc"}`))
			if body != `{"Id":"abc"}"I"` {
				t.Errorf("inspect body = %q", body)
			}
		})
	}
}

// Everything the hook should not see, and everything it declines, comes back as
// the daemon wrote it.
func TestProxyRelaysOtherResponsesUnchanged(t *testing.T) {
	big := `["` + strings.Repeat("x", maxPortViewBody) + `"]`
	tests := []struct {
		name, path, raw, want string
		skip                  bool
	}{
		{"another path", "/containers/abc/stats", fixed(`{"a":1}`), `{"a":1}`, false},
		{"not 200", "/containers/json", "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: 7\r\n\r\n{\"m\":1}", `{"m":1}`, false},
		{"not JSON", "/containers/json", "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nplain", "plain", false},
		{"over the bound", "/containers/json", chunked(big), big, false},
		{"declined", "/containers/json", fixed(`["a"]`), `["a"]`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := &markView{skip: tt.skip}
			_, got := get(t, &Proxy{PortView: view}, tt.path, tt.raw)
			if got != tt.want {
				t.Errorf("relayed %.60q (%d bytes), want %.60q (%d bytes)", got, len(got), tt.want, len(tt.want))
			}
			if tt.name != "declined" && len(view.seen) != 0 {
				t.Errorf("the hook was asked about %.60q", view.seen)
			}
		})
	}
}
