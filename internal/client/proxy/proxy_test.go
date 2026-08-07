package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon stands in for the workspace's dockerd. Each connection is served
// as a plain HTTP/1.1 server, so the proxy is exercised against real framing.
type fakeDaemon struct {
	listener net.Listener

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Body   string
}

func (d *fakeDaemon) record(r recordedRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, r)
}

func (d *fakeDaemon) recorded() []recordedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedRequest(nil), d.requests...)
}

// startDaemon runs a fake daemon whose handler is supplied by the test.
func startDaemon(t *testing.T, handle func(*fakeDaemon, *http.Request, net.Conn, *bufio.Reader)) *fakeDaemon {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	d := &fakeDaemon{listener: l}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				req, err := http.ReadRequest(reader)
				if err != nil {
					return
				}
				body, _ := io.ReadAll(req.Body)
				d.record(recordedRequest{Method: req.Method, Path: req.URL.Path, Body: string(body)})
				handle(d, req, conn, reader)
			}()
		}
	}()
	return d
}

// tcpDialer connects the proxy to the fake daemon.
type tcpDialer struct{ addr string }

func (t *tcpDialer) DialDocker(context.Context) (io.ReadWriteCloser, error) {
	return net.Dial("tcp", t.addr)
}

// startProxy serves the proxy on a loopback listener and returns its address.
func startProxy(t *testing.T, p *Proxy) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	go p.Serve(t.Context(), l)
	return l.Addr().String()
}

// respondJSON writes a simple JSON response.
func respondJSON(conn net.Conn, status int, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

func TestProxyForwardsRequests(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 200, `[{"Id":"abc"}]`)
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	resp, err := http.Get("http://" + addr + "/v1.51/containers/json")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `[{"Id":"abc"}]` {
		t.Errorf("body = %q", body)
	}
	if got := daemon.recorded(); len(got) != 1 || got[0].Path != "/v1.51/containers/json" {
		t.Errorf("daemon saw %+v", got)
	}
}

// The rewriter must see container-create bodies, and only those.
func TestProxyRewritesContainerCreate(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 201, `{"Id":"new"}`)
	})

	p := &Proxy{
		Dialer: &tcpDialer{daemon.listener.Addr().String()},
		Rewriter: rewriterFunc(func(_ context.Context, body []byte) ([]byte, error) {
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				return nil, err
			}
			payload["Rewritten"] = true
			return json.Marshal(payload)
		}),
	}
	addr := startProxy(t, p)

	resp, err := http.Post("http://"+addr+"/v1.51/containers/create", "application/json",
		strings.NewReader(`{"Image":"alpine"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	got := daemon.recorded()
	if len(got) != 1 {
		t.Fatalf("daemon saw %d requests, want 1", len(got))
	}
	if !strings.Contains(got[0].Body, `"Rewritten":true`) {
		t.Errorf("daemon received %q, which was not rewritten", got[0].Body)
	}
	// The rewritten body has a known length; a stale chunked framing here
	// would misframe it and the daemon would reject the request.
	if !strings.Contains(got[0].Body, `"Image":"alpine"`) {
		t.Errorf("daemon received %q, which lost the original fields", got[0].Body)
	}
}

func TestProxyDoesNotRewriteOtherRequests(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 200, `{}`)
	})

	called := false
	p := &Proxy{
		Dialer: &tcpDialer{daemon.listener.Addr().String()},
		Rewriter: rewriterFunc(func(_ context.Context, body []byte) ([]byte, error) {
			called = true
			return body, nil
		}),
	}
	addr := startProxy(t, p)

	// A path merely containing the words must not trigger it, nor a GET.
	for _, path := range []string{"/v1.51/containers/json", "/v1.51/containers/create/foo", "/images/create"} {
		resp, err := http.Post("http://"+addr+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
	}
	if called {
		t.Error("the rewriter was invoked for a request that is not a container create")
	}
}

// An HTTP upgrade means everything after the response head is raw bytes in
// both directions. This is docker exec, attach, and buildx's /session -- so
// docker build depends on it (ADR 0009).
func TestProxyPassesThroughHijackedConnections(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, reader *bufio.Reader) {
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		// Echo raw bytes, exactly as an exec stream would carry them.
		buf := make([]byte, 1024)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				conn.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	io.WriteString(conn, "POST /v1.51/exec/abc/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status = %q, want 101", statusLine)
	}
	// Drain the remaining headers.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	const payload = "raw stream bytes"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("write after upgrade: %v", err)
	}
	buf := make([]byte, len(payload))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatalf("read after upgrade: %v", err)
	}
	if string(buf) != payload {
		t.Errorf("echoed %q, want %q", buf, payload)
	}
}

// Streaming responses -- /events, /build, logs with follow -- must reach the
// client as they are produced, not when the response completes. A proxy that
// buffers looks correct for docker ps and then hangs docker events forever.
func TestProxyStreamsResponsesIncrementally(t *testing.T) {
	release := make(chan struct{})
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
		writeChunk := func(s string) {
			fmt.Fprintf(conn, "%x\r\n%s\r\n", len(s), s)
		}
		writeChunk(`{"status":"first"}` + "\n")
		<-release // hold the response open
		writeChunk(`{"status":"second"}` + "\n")
		io.WriteString(conn, "0\r\n\r\n")
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	resp, err := http.Get("http://" + addr + "/v1.51/events")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// The first event must arrive while the daemon is still holding the
	// response open.
	first := make([]byte, len(`{"status":"first"}`+"\n"))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, first)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reading the first event: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the first event never arrived; the proxy is buffering the response")
	}

	if !strings.Contains(string(first), "first") {
		t.Errorf("first event = %q", first)
	}
	close(release)
}

// Several requests on one client connection must all be served, because the
// Docker CLI reuses connections.
func TestProxyHandlesKeepAlive(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 200, `{"ok":true}`)
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	for i := range 3 {
		resp, err := client.Get("http://" + addr + "/v1.51/_ping")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := len(daemon.recorded()); got != 3 {
		t.Errorf("daemon saw %d requests, want 3", got)
	}
}

// A rewriter failure has to reach the user as a Docker API error, or the CLI
// reports "unexpected EOF" and the actual reason is lost.
func TestProxyReportsRewriteFailures(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 201, `{}`)
	})
	p := &Proxy{
		Dialer: &tcpDialer{daemon.listener.Addr().String()},
		Rewriter: rewriterFunc(func(context.Context, []byte) ([]byte, error) {
			return nil, fmt.Errorf("cannot export D:\\data")
		}),
	}
	addr := startProxy(t, p)

	resp, err := http.Post("http://"+addr+"/v1.51/containers/create", "application/json",
		strings.NewReader(`{"Image":"alpine"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("error body is not JSON the CLI can read: %q", body)
	}
	if !strings.Contains(msg.Message, `D:\data`) {
		t.Errorf("message = %q, which loses the underlying reason", msg.Message)
	}
}

type rewriterFunc func(context.Context, []byte) ([]byte, error)

func (f rewriterFunc) ContainerCreate(ctx context.Context, body []byte) ([]byte, error) {
	return f(ctx, body)
}

// Attach does not always negotiate with 101. When the client does not request
// a protocol upgrade, the daemon answers 200 with a docker stream content type
// and then writes raw frames. Treating that as an ordinary response is wrong
// in the worst way: `docker run` exits 0 having printed nothing, because the
// container's output was framed as an HTTP body nobody read.
//
// The integration suite found this -- every test that read container stdout
// failed empty while every test that did not passed.
func TestProxyHijacksOnDockerStreamContentType(t *testing.T) {
	for _, contentType := range []string{
		"application/vnd.docker.raw-stream",
		"application/vnd.docker.multiplexed-stream",
		"application/vnd.docker.raw-stream; charset=utf-8",
	} {
		t.Run(contentType, func(t *testing.T) {
			const output = "hello from the container\n"
			daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\n\r\n", contentType)
				io.WriteString(conn, output)
			})
			addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			io.WriteString(conn, "POST /v1.51/containers/abc/attach?stream=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")

			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(conn)

			// Drain the head.
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("reading head: %v", err)
				}
				if strings.TrimSpace(line) == "" {
					break
				}
			}

			buf := make([]byte, len(output))
			if _, err := io.ReadFull(reader, buf); err != nil {
				t.Fatalf("the container's output never arrived: %v", err)
			}
			if string(buf) != output {
				t.Errorf("got %q, want %q", buf, output)
			}
		})
	}
}

// A non-standard reason phrase must survive: attach answers "101 UPGRADED",
// and a client matching on it would be misled by a rewritten one.
func TestProxyPreservesTheReasonPhrase(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		io.WriteString(conn, "x")
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /v1.51/exec/abc/start HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	statusLine, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("reading status: %v", err)
	}
	if !strings.Contains(statusLine, "UPGRADED") {
		t.Errorf("status = %q, want the daemon's own reason phrase", statusLine)
	}
}

// An ordinary JSON response must NOT be treated as a hijack, or every normal
// API call would stop being parseable.
func TestProxyDoesNotHijackOrdinaryResponses(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, 200, `{"ok":true}`)
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://" + addr + "/v1.51/_ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
}

// halfCloseStream is an upstream that distinguishes a half-close from a full
// close, the way an SSH session stream does.
type halfCloseStream struct {
	io.Reader
	io.Writer
	writeClosed chan struct{}
	closed      chan struct{}
	once        sync.Once
	writeOnce   sync.Once
}

func (h *halfCloseStream) CloseWrite() error {
	h.writeOnce.Do(func() { close(h.writeClosed) })
	return nil
}

func (h *halfCloseStream) Close() error {
	h.once.Do(func() { close(h.closed) })
	return nil
}

type halfCloseDialer struct{ stream *halfCloseStream }

func (d *halfCloseDialer) DialDocker(context.Context) (io.ReadWriteCloser, error) {
	return d.stream, nil
}

// The bug this pins down: `docker run` without -i closes its stdin as soon as
// the attach is established. A proxy that responded by closing the whole
// upstream would tear down the session carrying the container's output, and
// the command would exit 0 having printed nothing at all.
//
// Every mount test in the integration suite failed this way -- empty output,
// zero exit -- while writes and port forwarding passed, because only the
// tests that read container stdout went through the attach path.
func TestProxyHalfClosesUpstreamWhenClientStopsWriting(t *testing.T) {
	// Upstream sends a hijack head, then holds the connection open.
	upstreamReads, upstreamWrites := io.Pipe()
	stream := &halfCloseStream{
		Reader:      upstreamReads,
		Writer:      io.Discard,
		writeClosed: make(chan struct{}),
		closed:      make(chan struct{}),
	}

	addr := startProxy(t, &Proxy{Dialer: &halfCloseDialer{stream}})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	io.WriteString(conn, "POST /v1.51/containers/abc/attach?stream=1 HTTP/1.1\r\nHost: docker\r\nContent-Length: 0\r\n\r\n")

	go func() {
		io.WriteString(upstreamWrites, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\n\r\n")
	}()

	// The client finishes sending and half-closes, exactly as `docker run`
	// without -i does.
	if c, ok := conn.(*net.TCPConn); ok {
		if err := c.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
	}

	select {
	case <-stream.writeClosed:
		// Correct: end-of-input signalled.
	case <-stream.closed:
		t.Fatal("the whole upstream was closed; the container's output stream would be torn down before it wrote anything")
	case <-time.After(5 * time.Second):
		t.Fatal("the client's half-close was never propagated upstream")
	}

	// The other direction must still be usable.
	const output = "output after the client stopped writing\n"
	go func() { io.WriteString(upstreamWrites, output) }()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading head: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	buf := make([]byte, len(output))
	if _, err := io.ReadFull(reader, buf); err != nil {
		t.Fatalf("output after half-close never arrived: %v", err)
	}
	if string(buf) != output {
		t.Errorf("got %q, want %q", buf, output)
	}
}

// `docker logs` uses the same content type as a hijacked attach, but is an
// ordinary chunked response. Splicing it raw hands the chunk-size lines to
// the client's stream demultiplexer, which reports
// "Unrecognized input header: 49" -- 49 being the ASCII '1' that starts a hex
// chunk length. The integration suite reported exactly that string.
func TestProxyDoesNotHijackChunkedDockerStreams(t *testing.T) {
	// One stdcopy frame: stdout, 5 bytes, "hello".
	frame := "\x01\x00\x00\x00\x00\x00\x00\x05hello"

	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		io.WriteString(conn, "HTTP/1.1 200 OK\r\n"+
			"Content-Type: application/vnd.docker.multiplexed-stream\r\n"+
			"Transfer-Encoding: chunked\r\n\r\n")
		fmt.Fprintf(conn, "%x\r\n%s\r\n0\r\n\r\n", len(frame), frame)
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://" + addr + "/v1.51/containers/abc/logs?stdout=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Go's client de-chunks, so the body must be the frame and nothing else.
	// If the proxy spliced it raw, the chunk-size line would be here too.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != frame {
		t.Errorf("body = %q, want the stdcopy frame alone %q", body, frame)
	}
}

// A response with a content length is likewise a normal response, not a
// takeover.
func TestProxyDoesNotHijackLengthDelimitedDockerStreams(t *testing.T) {
	const payload = "not a hijack"
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s",
			rawStreamType, len(payload), payload)
	})
	addr := startProxy(t, &Proxy{Dialer: &tcpDialer{daemon.listener.Addr().String()}})

	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()

	resp, err := client.Get("http://" + addr + "/v1.51/containers/abc/logs")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != payload {
		t.Errorf("body = %q, want %q", body, payload)
	}
}

// Serve must return promptly when its context is cancelled, even with an idle
// keep-alive connection still open.
//
// This is the shape of a real hang, and the reason it is worth a test rather
// than a comment. A Docker client keeps its connection alive between requests,
// so the handler that served the last one sits blocked reading the next. When
// the client is a separate process that is about to exit, the socket closes and
// the handler unblocks. But the EMBEDDED Docker CLI runs in this very process,
// so nothing closes it -- and `remote-docker docker run` sat for minutes after
// the container had exited, waiting for a peer that was itself waiting to be
// told to go away.
func TestServeReturnsPromptlyWithAnIdleKeepAliveConnection(t *testing.T) {
	daemon := startDaemon(t, func(_ *fakeDaemon, _ *http.Request, conn net.Conn, _ *bufio.Reader) {
		respondJSON(conn, http.StatusOK, `{"ok":true}`)
	})

	p := &Proxy{Dialer: &tcpDialer{addr: daemon.listener.Addr().String()}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- p.Serve(ctx, l) }()

	// A keep-alive client, deliberately NOT closed afterwards -- standing in
	// for the embedded CLI's transport, which lives as long as we do.
	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Get("http://" + l.Addr().String() + "/_ping")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Shut down the way Session.Close does: cancel, then close the listener.
	cancel()
	_ = l.Close()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s; a handler is still blocked " +
			"reading an idle keep-alive connection that nothing will ever close")
	}
}
