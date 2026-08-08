// Package proxy exposes a local Docker API endpoint that forwards to the
// workspace daemon over SSH, rewriting bind mounts on the way.
//
// This is what lets the real `docker` CLI, Compose, Testcontainers and IDE
// integrations work unmodified: they speak the Engine API, so the translation
// belongs at the API rather than in a command wrapper (ADR 0005).
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Dialer opens a fresh connection to the workspace's Docker socket.
//
// In production this is an SSH session running `docker system dial-stdio`;
// in tests it is a plain net.Dial at a local server.
type Dialer interface {
	DialDocker(ctx context.Context) (io.ReadWriteCloser, error)
}

// Rewriter mutates a container-create body.
type Rewriter interface {
	ContainerCreate(ctx context.Context, body []byte) ([]byte, error)
}

// Logger reports what the proxy is doing. Nil means silence.
type Logger interface {
	Printf(format string, args ...any)
}

// Proxy serves the Docker API on a local listener.
type Proxy struct {
	Dialer   Dialer
	Rewriter Rewriter
	Log      Logger

	wg sync.WaitGroup

	// live tracks accepted connections so shutdown can close them.
	//
	// Needed because a Docker client keeps its connection alive between
	// requests, so a handler that has finished serving one sits blocked
	// reading the next. Usually harmless -- the client process exits and the
	// socket closes -- but the EMBEDDED CLI runs in this very process, so
	// nothing ever closes it, and `remote-docker docker run` took three
	// minutes to exit while Serve waited for a peer that was itself waiting
	// to be told to go away.
	mu       sync.Mutex
	live     map[net.Conn]struct{}
	shutdown bool
}

// Serve accepts connections until l is closed.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) error {
	// Closing live connections on cancellation is what makes shutdown prompt.
	// An idle keep-alive connection has nothing to say and will not notice
	// the context; the handler blocked on it only unblocks when the socket
	// underneath it goes.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			p.closeLive()
		case <-done:
		}
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				p.wg.Wait()
				return nil
			}
			return fmt.Errorf("proxy: accept: %w", err)
		}
		if !p.track(conn) {
			// Shutdown began between Accept and here.
			_ = conn.Close()
			continue
		}
		p.wg.Go(func() {
			defer p.untrack(conn)
			defer conn.Close()
			p.handleConn(ctx, conn)
		})
	}
}

// track registers a connection, reporting false once shutdown has begun.
func (p *Proxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.shutdown {
		return false
	}
	if p.live == nil {
		p.live = map[net.Conn]struct{}{}
	}
	p.live[conn] = struct{}{}
	return true
}

// closing reports whether shutdown has begun, so a failure caused by our own
// teardown is not reported as one caused by anything else.
func (p *Proxy) closing() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shutdown
}

func (p *Proxy) untrack(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.live, conn)
}

// closeLive drops every accepted connection, including hijacked ones.
//
// Deliberately brutal: by the time this runs the session is going away, and a
// container's attached stream is about to lose the tunnel underneath it
// regardless. Ending it here is the same outcome, minutes sooner.
func (p *Proxy) closeLive() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shutdown = true
	for conn := range p.live {
		_ = conn.Close()
	}
	clear(p.live)
}

// handleConn services one client connection, which may carry several requests.
//
// Each request gets its own connection to the workspace daemon rather than
// reusing one. Keep-alive multiplexing across a shared upstream is where
// proxies of this shape go wrong -- a hijacked or streaming response leaves
// the upstream connection in a state the next request cannot use -- and an
// extra SSH channel is cheap next to being subtly wrong under `docker exec`.
func (p *Proxy) handleConn(ctx context.Context, client net.Conn) {
	reader := bufio.NewReader(client)

	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			// Once shutdown has begun every connection is closed underneath
			// its handler on purpose, so the resulting read failure describes
			// the shutdown rather than a fault. Reporting it turned a clean
			// exit into two alarming lines about pipes that had ended.
			if err != io.EOF && !errors.Is(err, net.ErrClosed) && !p.closing() {
				p.logf("reading request: %v", err)
			}
			return
		}

		keepGoing, err := p.forward(ctx, client, reader, req)
		if err != nil {
			if p.closing() {
				// The client is going away with us; there is nobody left to
				// read an error and nothing left to do about it.
				return
			}
			p.logf("%s %s: %v", req.Method, req.URL.Path, err)
			writeError(client, err)
			return
		}
		if !keepGoing {
			return
		}
	}
}

// forward sends one request upstream and relays the response. It reports
// whether the connection can carry another request.
func (p *Proxy) forward(ctx context.Context, client net.Conn, clientReader *bufio.Reader, req *http.Request) (bool, error) {
	upstream, err := p.Dialer.DialDocker(ctx)
	if err != nil {
		return false, fmt.Errorf("connecting to the workspace daemon: %w", err)
	}
	defer upstream.Close()

	if isContainerCreate(req) && p.Rewriter != nil {
		if err := p.rewriteBody(ctx, req); err != nil {
			return false, err
		}
	}

	// The daemon is reached over a stream we own end to end, so there is no
	// proxy in front of it and no reason to keep the request's original
	// framing headers.
	req.Close = false
	if err := req.Write(upstream); err != nil {
		return false, fmt.Errorf("sending request: %w", err)
	}

	upstreamReader := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamReader, req)
	if err != nil {
		return false, fmt.Errorf("reading response: %w", err)
	}
	defer resp.Body.Close()

	// A hijack -- `docker exec`, `attach`, and buildx's /session, which carries
	// gRPC -- means everything after the response head is raw bytes in both
	// directions. Nothing may parse or buffer it after this point.
	if isHijack(resp) {
		// Only the head. resp.Write would copy the body too, and for a
		// content-type hijack the body IS the stream -- it would be consumed
		// here, one direction only, and the splice below would never run.
		if err := writeHead(client, resp); err != nil {
			return false, fmt.Errorf("writing hijack response: %w", err)
		}
		splice(client, clientReader, upstream, upstreamReader)
		return false, nil
	}

	// Streaming responses -- /events, /build, logs with follow -- are copied
	// as they arrive. resp.Write does not buffer the body, so a chunk read
	// from the daemon becomes a write to the client.
	if err := resp.Write(client); err != nil {
		return false, fmt.Errorf("writing response: %w", err)
	}
	return !resp.Close && !req.Close, nil
}

// rewriteBody replaces the request body with a rewritten one.
func (p *Proxy) rewriteBody(ctx context.Context, req *http.Request) error {
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return fmt.Errorf("reading container create body: %w", err)
	}

	rewritten, err := p.Rewriter.ContainerCreate(ctx, body)
	if err != nil {
		return err
	}

	req.Body = io.NopCloser(strings.NewReader(string(rewritten)))
	req.ContentLength = int64(len(rewritten))
	// The original may have arrived chunked; the rewritten body has a known
	// length, and leaving a stale Transfer-Encoding would misframe it.
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	return nil
}

// closeWriteOrNothing signals end-of-input on a stream that supports it.
//
// Deliberately does nothing when the stream cannot half-close: ending the
// whole stream is not a safe fallback here, because the other direction is
// still carrying the response we are waiting for.
func closeWriteOrNothing(s io.ReadWriteCloser) {
	if cw, ok := s.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// writeHead writes a response's status line and headers, and nothing else.
//
// resp.Status is preferred over deriving the text from the code, because the
// daemon's reason phrases are not always the standard ones -- attach answers
// "101 UPGRADED" -- and a client matching on it would be misled.
func writeHead(w io.Writer, resp *http.Response) error {
	status := resp.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	var head strings.Builder
	fmt.Fprintf(&head, "HTTP/1.1 %s\r\n", status)
	if err := resp.Header.Write(&head); err != nil {
		return err
	}
	head.WriteString("\r\n")

	_, err := io.WriteString(w, head.String())
	return err
}

// Docker's content types for a hijacked stream. The daemon answers an attach
// with one of these and then treats the connection as raw bytes.
const (
	rawStreamType         = "application/vnd.docker.raw-stream"
	multiplexedStreamType = "application/vnd.docker.multiplexed-stream"
)

// isHijack reports whether the daemon has taken the connection over.
//
// 101 is the obvious case and the one everyone thinks of. It is not the only
// one: attach negotiates by content type when the client does not request a
// protocol upgrade, answering 200 with a docker stream content type and then
// writing raw frames. Treating that as an ordinary response is exactly wrong,
// and wrong in a way that looks like success -- `docker run` exits 0 having
// printed nothing, because the container's output was framed as an HTTP body
// nobody was reading. Found by the integration suite: every test that read
// container stdout failed empty while every test that did not passed.
func isHijack(resp *http.Response) bool {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return true
	}
	switch contentType(resp) {
	case rawStreamType, multiplexedStreamType:
		// The content type alone is not enough. `docker logs` uses the very
		// same one for an ordinary chunked response, and splicing that raw
		// hands the chunk-size lines to the client's stream demultiplexer,
		// which reports "Unrecognized input header: 49" -- 49 being the ASCII
		// '1' that starts a hex chunk length.
		//
		// A hijack is the case where the daemon frames nothing itself: no
		// content length and no transfer encoding, just bytes until the
		// connection ends.
		return resp.ContentLength < 0 && len(resp.TransferEncoding) == 0
	default:
		return false
	}
}

// contentType returns the media type with any parameters stripped.
func contentType(resp *http.Response) string {
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// isContainerCreate matches POST /containers/create and its versioned form,
// /v1.51/containers/create.
func isContainerCreate(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	return strings.HasSuffix(req.URL.Path, "/containers/create")
}

// splice copies raw bytes in both directions after an upgrade, including
// anything the buffered readers pulled in ahead of the switch.
func splice(client net.Conn, clientReader *bufio.Reader, upstream io.ReadWriteCloser, upstreamReader *bufio.Reader) {
	var wg sync.WaitGroup

	wg.Go(func() {
		// Buffered bytes first, or the first frame of the upgraded protocol
		// is silently dropped.
		if n := clientReader.Buffered(); n > 0 {
			if buffered, err := clientReader.Peek(n); err == nil {
				_, _ = upstream.Write(buffered)
				_, _ = clientReader.Discard(n)
			}
		}
		_, _ = io.Copy(upstream, clientReader)

		// Half-close, never a full close. `docker run` without -i closes its
		// stdin the moment the attach is established; closing the whole
		// upstream in response would tear down the session carrying the
		// container's output, and the command would exit 0 having printed
		// nothing. Only signal end-of-input.
		closeWriteOrNothing(upstream)
	})

	wg.Go(func() {
		if n := upstreamReader.Buffered(); n > 0 {
			if buffered, err := upstreamReader.Peek(n); err == nil {
				_, _ = client.Write(buffered)
				_, _ = upstreamReader.Discard(n)
			}
		}
		_, _ = io.Copy(client, upstreamReader)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			client.Close()
		}
	})

	wg.Wait()
}

// writeError reports a proxy-level failure in the shape the Docker CLI
// expects, so it prints a message rather than "unexpected EOF".
func writeError(w io.Writer, err error) {
	body := fmt.Sprintf("{\"message\":%q}", err.Error())
	_, _ = fmt.Fprintf(w, "HTTP/1.1 500 Internal Server Error\r\n"+
		"Content-Type: application/json\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n\r\n%s", len(body), body)
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log.Printf(format, args...)
	}
}
