// Package proxy exposes a local Docker API endpoint that forwards to the
// workspace daemon over SSH, rewriting bind mounts on the way (ADR 0005).
package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/tunnel"
)

// Dialer opens a fresh connection to the workspace's Docker socket.
type Dialer interface {
	DialDocker(ctx context.Context) (io.ReadWriteCloser, error)
}

// Rewriter mutates a container-create body.
type Rewriter interface {
	ContainerCreate(ctx context.Context, body []byte) ([]byte, error)
}

// Proxy serves the Docker API on a local listener.
type Proxy struct {
	Dialer   Dialer
	Rewriter Rewriter
	Log      *slog.Logger

	// Control answers ControlPrefix. Nil unless this session is the daemon.
	Control Control

	wg sync.WaitGroup

	// live lets shutdown close idle keep-alive connections, which nothing
	// else closes when the client is the embedded CLI in this process.
	mu       sync.Mutex
	live     map[net.Conn]struct{}
	shutdown bool
}

// Serve accepts connections until l is closed.
func (p *Proxy) Serve(ctx context.Context, l net.Listener) error {
	// A handler blocked on an idle connection ignores ctx; closing it is the
	// only way to unblock it.
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

// clientGone reports an error that only means the other end hung up. Routine:
// a client abandons /wait once attach says the container is gone.
func clientGone(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		peerGone(err)
}

// closing reports whether shutdown has begun, so our own teardown is not
// logged as a fault.
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

// closeLive drops every accepted connection, hijacked ones included: the
// session is going and would take them anyway.
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
// Each request gets its own upstream: a hijacked or streamed response leaves a
// shared one unusable.
func (p *Proxy) handleConn(ctx context.Context, client net.Conn) {
	reader := bufio.NewReader(client)

	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if err != io.EOF && !clientGone(err) && !p.closing() {
				p.log().Warn("reading request", "err", err)
			}
			return
		}

		keepGoing, err := p.forward(ctx, client, reader, req)
		if err != nil {
			if p.closing() || clientGone(err) {
				return
			}
			p.log().Warn("proxying a request", "method", req.Method, "path", req.URL.Path, "err", err)
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
	if isControl(req) {
		p.serveControl(client, req)
		return false, nil
	}

	// Traced in parts: the dial is ours, everything after is the daemon's.
	started := time.Now()
	upstream, err := p.Dialer.DialDocker(ctx)
	if err != nil {
		return false, fmt.Errorf("connecting to the workspace daemon: %w", err)
	}
	defer upstream.Close()
	dialed := time.Now()

	var sent, headed, written time.Time
	if traceEnabled {
		defer func() {
			// A hijack never sets written.
			body := "stream"
			if !written.IsZero() {
				body = written.Sub(headed).Round(time.Millisecond).String()
			}
			p.log().Info("trace",
				"method", req.Method, "path", req.URL.Path,
				"dial", dialed.Sub(started).Round(time.Millisecond),
				"send", sent.Sub(dialed).Round(time.Millisecond),
				"wait", headed.Sub(sent).Round(time.Millisecond),
				"body", body,
				"total", time.Since(started).Round(time.Millisecond))
		}()
	}

	if isContainerCreate(req) && p.Rewriter != nil {
		if err := p.rewriteBody(ctx, req); err != nil {
			return false, err
		}
	}

	req.Close = false
	if err := req.Write(upstream); err != nil {
		return false, fmt.Errorf("sending request: %w", err)
	}
	sent = time.Now()

	upstreamReader := bufio.NewReader(upstream)
	resp, err := http.ReadResponse(upstreamReader, req)
	if err != nil {
		return false, fmt.Errorf("reading response: %w", err)
	}
	headed = time.Now()
	defer resp.Body.Close()

	// After a hijack (exec, attach, buildx's /session) everything is raw bytes
	// both ways.
	if isHijack(resp) {
		// Only the head: for a content-type hijack the body IS the stream, and
		// resp.Write would consume it one way.
		if err := writeHead(client, resp); err != nil {
			return false, fmt.Errorf("writing hijack response: %w", err)
		}
		splice(client, clientReader, upstream, upstreamReader)
		return false, nil
	}

	// resp.Write does not buffer, so /events, /build and logs -f stream.
	err = resp.Write(client)
	written = time.Now()
	if err != nil {
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
	// A stale chunked Transfer-Encoding would misframe the new body.
	req.TransferEncoding = nil
	req.Header.Del("Content-Length")
	return nil
}

// writeHead writes a response's status line and headers only, keeping the
// daemon's own reason phrase ("101 UPGRADED").
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

// Docker's content types for a hijacked stream.
const (
	rawStreamType         = "application/vnd.docker.raw-stream"
	multiplexedStreamType = "application/vnd.docker.multiplexed-stream"
)

// isHijack reports whether the daemon has taken the connection over: 101, or
// 200 with a docker stream content type. Missing one, `docker run` exits 0
// having printed nothing.
func isHijack(resp *http.Response) bool {
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return true
	}
	switch contentType(resp) {
	case rawStreamType, multiplexedStreamType:
		// Only when unframed: `docker logs` sends the same type chunked, and
		// splicing that gives "Unrecognized input header: 49".
		return resp.ContentLength < 0 && len(resp.TransferEncoding) == 0
	default:
		return false
	}
}

// contentType returns the media type with any parameters stripped.
func contentType(resp *http.Response) string {
	ct, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
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
		// Buffered bytes first, or the first upgraded frame is lost.
		if n := clientReader.Buffered(); n > 0 {
			if buffered, err := clientReader.Peek(n); err == nil {
				_, _ = upstream.Write(buffered)
				_, _ = clientReader.Discard(n)
			}
		}
		_, _ = io.Copy(upstream, clientReader)

		// Half-close only: `docker run` without -i closes stdin at once, and a
		// full close would drop the container's output.
		tunnel.CloseWrite(upstream)
	})

	wg.Go(func() {
		if n := upstreamReader.Buffered(); n > 0 {
			if buffered, err := upstreamReader.Peek(n); err == nil {
				_, _ = client.Write(buffered)
				_, _ = upstreamReader.Discard(n)
			}
		}
		_, _ = io.Copy(client, upstreamReader)

		// Upstream is done, so a client that cannot half-close is closed.
		tunnel.CloseWriteOrClose(client)
	})

	wg.Wait()
}

// writeError reports a proxy-level failure in the shape the Docker CLI
// expects, so it prints a message rather than "unexpected EOF".
func writeError(w io.Writer, err error) {
	writeControl(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
}

// log is the proxy's logger, or silence. See logx.Or.
func (p *Proxy) log() *slog.Logger {
	return logx.Or(p.Log)
}

// TraceEnv turns on one timing line per Docker API request. An environment
// variable because the background session, which forwards, takes no flags.
const TraceEnv = "REMOTE_DOCKER_TRACE"

var traceEnabled = os.Getenv(TraceEnv) != ""

// Tracing reports whether this process is tracing. Setting TraceEnv on a
// command does nothing unless the session was started with it.
func Tracing() bool { return traceEnabled }
