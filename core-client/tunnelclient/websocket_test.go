package tunnelclient

// Dialling a workspace through a proxy.
//
// The address test is here because the first version of this package failed
// end to end without failing here: the library reports a placeholder
// RemoteAddr with no port in it, known_hosts looks a host key up by that
// address, and every host-key check failed before it could compare anything.
// A test that only moves bytes never sees it.

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// echoWS accepts a WebSocket and reflects what it is sent.
func echoWS(t *testing.T, tlsServer bool) *httptest.Server {
	t.Helper()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		buf := make([]byte, 32)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	})

	srv := httptest.NewUnstartedServer(h)
	if tlsServer {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv
}

func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/tunnel"
}

// addrOf is what the caller passes as WebSocketOptions.Addr: the host and port it
// already worked out when it decided where to connect.
func addrOf(srv *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
}

// The connection has to name where it went, because known_hosts asks it.
func TestTheConnectionReportsAnAddressWithAPort(t *testing.T) {
	srv := echoWS(t, false)

	dial, err := WebSocketDialer(WebSocketOptions{URL: wsURL(srv), Addr: addrOf(srv)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	addr := conn.RemoteAddr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("RemoteAddr %q cannot be split: %v -- known_hosts will refuse every connection", addr, err)
	}
	if host == "" || port == "" {
		t.Errorf("RemoteAddr %q has an empty half", addr)
	}
}

// Addr is required: without it nothing can look up a host key, and failing at
// construction says so where a handshake error would not.
func TestAddrIsRequired(t *testing.T) {
	if _, err := WebSocketDialer(WebSocketOptions{URL: "wss://ws.example/tunnel"}); err == nil {
		t.Error("a dialler with no Addr was accepted")
	}
}

// Bytes in both directions.
func TestItCarriesAStream(t *testing.T) {
	srv := echoWS(t, false)

	dial, err := WebSocketDialer(WebSocketOptions{URL: wsURL(srv), Addr: addrOf(srv)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dial(context.Background())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("through the proxy")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "through the proxy" {
		t.Errorf("read %q", got)
	}
}

// A certificate nothing trusts is refused, and Insecure is what accepts it.
// Both asserted, so the flag cannot quietly become the default.
func TestCertificateVerification(t *testing.T) {
	srv := echoWS(t, true)
	url := wsURL(srv)

	dial, err := WebSocketDialer(WebSocketOptions{URL: url, Addr: addrOf(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dial(context.Background()); err == nil {
		conn.Close()
		t.Error("an untrusted certificate was accepted")
	}

	dial, err = WebSocketDialer(WebSocketOptions{URL: url, Addr: addrOf(srv), Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dial(context.Background())
	if err != nil {
		t.Fatalf("Insecure did not accept the certificate: %v", err)
	}
	conn.Close()
}

// A CA file makes the same certificate acceptable, which is the case for a
// proxy holding a private one.
func TestACAFileVerifies(t *testing.T) {
	srv := echoWS(t, true)

	pem := certPEM(t, srv)
	dial, err := WebSocketDialer(WebSocketOptions{URL: wsURL(srv), Addr: addrOf(srv), CAFile: pem})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dial(context.Background())
	if err != nil {
		t.Fatalf("the CA file did not verify the server: %v", err)
	}
	conn.Close()
}

// A CA file that is not one is refused when it is read, not left to fail as a
// connection error later: falling back to the system roots would hide the
// mistake behind something that works until the day it should not have.
func TestABadCAFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "not-a-ca.pem")
	if err := os.WriteFile(empty, []byte("nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := WebSocketDialer(WebSocketOptions{URL: "wss://ws.example/tunnel", Addr: "ws.example:443", CAFile: empty}); err == nil {
		t.Error("a file holding no certificate was accepted as a CA")
	}
	if _, err := WebSocketDialer(WebSocketOptions{URL: "wss://ws.example/tunnel", Addr: "ws.example:443", CAFile: filepath.Join(dir, "missing")}); err == nil {
		t.Error("a missing CA file was accepted")
	}
}

// certPEM writes the test server's certificate out so it can be trusted as a
// root, which is what a self-signed proxy certificate is.
func certPEM(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("the test server has no certificate")
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
