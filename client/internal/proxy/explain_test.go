package proxy

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const refusedMessage = "error while mounting volume '/var/lib/docker/volumes/rd-1a2b3c4d-0f9e8d7c6b5a4f3e/_data': failed to mount local volume: mount :/m/0f9e8d7c6b5a4f3e:/var/lib/docker/volumes/rd-1a2b3c4d-0f9e8d7c6b5a4f3e/_data, data: addr=127.0.0.1,port=40123,nfsvers=3: connection refused"

func jsonMessage(t *testing.T, msg string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"message": msg})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// post sends a bodiless POST through a proxy fronting a daemon that answers
// every request with the given status, content type and body, and returns the
// status and the body as relayed.
func post(t *testing.T, p *Proxy, path string, status int, contentType, body string) (int, string) {
	t.Helper()
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		io.WriteString(conn, "HTTP/1.1 "+http.StatusText(status)+"\r\n")
	})
	_ = daemon

	d := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		head := "HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n"
		if contentType != "" {
			head += "Content-Type: " + contentType + "\r\n"
		}
		head += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
		io.WriteString(conn, head+body)
	})
	p.Dialer = &tcpDialer{d.listener.Addr().String()}
	addr := startProxy(t, p)

	resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if int64(len(got)) != resp.ContentLength {
		t.Errorf("Content-Length %d does not match the %d bytes relayed", resp.ContentLength, len(got))
	}
	return resp.StatusCode, string(got)
}

func explainer(calls *[]string) func(volume, client string, port int) (string, string, bool) {
	return func(volume, client string, port int) (string, string, bool) {
		*calls = append(*calls, volume+" "+client+" "+strconv.Itoa(port))
		return "volume " + volume + ": explained", "do the thing", true
	}
}

func TestProxyExplainsAMountFailureOnStart(t *testing.T) {
	for _, path := range []string{
		"/containers/abc/start",
		"/v1.51/containers/abc/start",
		"/v1.51/containers/abc/restart",
	} {
		t.Run(path, func(t *testing.T) {
			var calls []string
			p := &Proxy{ExplainMountFailure: explainer(&calls)}
			body := `{"message":` + quote(t, refusedMessage) + `,"extra":1}`

			status, got := post(t, p, path, 500, "application/json", body)

			if status != 500 {
				t.Errorf("status = %d, want 500", status)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(got), &fields); err != nil {
				t.Fatalf("relayed body is not JSON: %q", got)
			}
			want := "volume rd-1a2b3c4d-0f9e8d7c6b5a4f3e: explained\n  fix: do the thing"
			if fields["message"] != want {
				t.Errorf("message = %q, want %q", fields["message"], want)
			}
			if fields["extra"] != float64(1) {
				t.Errorf("other fields were lost: %q", got)
			}
			if len(calls) != 1 || calls[0] != "rd-1a2b3c4d-0f9e8d7c6b5a4f3e 1a2b3c4d 40123" {
				t.Errorf("hook calls = %v", calls)
			}
		})
	}
}

func quote(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Anything the explainer does not claim is relayed exactly as the daemon sent it.
func TestProxyLeavesOtherResponsesAlone(t *testing.T) {
	matching := jsonMessage(t, refusedMessage)
	tests := []struct {
		name        string
		path        string
		status      int
		contentType string
		body        string
		hook        func(string, string, int) (string, string, bool)
	}{
		{"non-matching 500", "/containers/abc/start", 500, "application/json", `{"message":"no such image"}`, nil},
		{"2xx", "/containers/abc/start", 204, "", "", nil},
		{"2xx with a matching body", "/containers/abc/start", 200, "application/json", matching, nil},
		{"stop", "/containers/abc/stop", 500, "application/json", matching, nil},
		{"non-JSON", "/containers/abc/start", 500, "text/plain", matching, nil},
		{"over the bound", "/containers/abc/start", 500, "application/json",
			`{"message":` + quote(t, refusedMessage+strings.Repeat(" ", maxExplainBody)) + `}`, nil},
		{"hook declines", "/containers/abc/start", 500, "application/json", matching,
			func(string, string, int) (string, string, bool) { return "", "", false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			hook := tt.hook
			if hook == nil {
				hook = explainer(&calls)
			}
			p := &Proxy{ExplainMountFailure: hook}

			status, got := post(t, p, tt.path, tt.status, tt.contentType, tt.body)

			if status != tt.status {
				t.Errorf("status = %d, want %d", status, tt.status)
			}
			if got != tt.body {
				t.Errorf("body changed:\n got %q\nwant %q", got, tt.body)
			}
			if tt.hook == nil && len(calls) != 0 {
				t.Errorf("hook was asked: %v", calls)
			}
		})
	}
}

func TestProxyWithoutAnExplainerRelaysTheDaemonsText(t *testing.T) {
	body := jsonMessage(t, refusedMessage)
	_, got := post(t, &Proxy{}, "/containers/abc/start", 500, "application/json", body)
	if got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}
