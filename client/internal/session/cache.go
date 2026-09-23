package session

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/lhns/remote-docker/client/internal/rewrite"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/core/workspace"
	"github.com/lhns/remote-docker/dircache"
)

// The client's end of the cache channel (ADR 0044), and this project's
// dircache.Store. One per session, shared by every share, and serialised.

// cacheChannel is the session's link to the workspace's union mounts.
type cacheChannel struct {
	stream io.ReadWriteCloser

	// codec is the payload encoding the workspace announced in its greeting.
	codec string

	mu sync.Mutex
	r  *bufio.Reader
}

// openCache establishes the channel and completes the version handshake.
func openCache(ctx context.Context, client *tunnelclient.Client) (*cacheChannel, error) {
	stream, r, reply, err := greet(ctx, client, cache.Command, cache.MaxFrame, cache.Version,
		func(reply cache.Reply) (int, bool) {
			if reply.Hello == nil {
				return 0, false
			}
			return reply.Hello.Version, true
		})
	if err != nil {
		return nil, err
	}

	c := &cacheChannel{stream: stream, r: r}
	// Chosen from what the AGENT announced, never from what this client can
	// produce: an older workspace announces none and refuses one.
	if reply.Hello.Accepts(cache.CodecZstd) {
		c.codec = cache.CodecZstd
	}
	return c, nil
}

// notServedError is a channel the workspace answered with something other
// than a greeting: an agent too old for the command, which the message names.
type notServedError struct{ command string }

func (e *notServedError) Error() string {
	return fmt.Sprintf("the workspace did not answer %q", e.command)
}

// silentError is a channel the workspace accepted and then said nothing on.
// Silence names no cause, so it reports only what was observed.
type silentError struct {
	command string
	after   time.Duration
}

func (e *silentError) Error() string {
	return fmt.Sprintf("the workspace accepted %q and then said nothing for %s", e.command, e.after)
}

// handshakeTimeout bounds every greeting, which is one round trip. Without it
// an agent that does not know the command (v0.5.1) runs it as a shell command
// that never exits, and the client hangs printing nothing.
const handshakeTimeout = 10 * time.Second

// cacheRefusal turns a failed cache channel into the tail of the sentence
// refusing a mount, with its remedy. The agent version is context, never the
// test.
func cacheRefusal(err error, agent string) error {
	var notServed *notServedError
	if errors.As(err, &notServed) {
		return fmt.Errorf("this workspace does not serve it%s%s", runningVersion(agent), rewrite.FixUpdateWorkspace)
	}
	var silent *silentError
	if errors.As(err, &silent) {
		// Not "try again": a v0.5.1 workspace never answers.
		return fmt.Errorf("%w\n  fix: use write=%s, which is served by the mount itself",
			silent, workspace.WriteThrough)
	}
	return err
}

// runningVersion names the agent answering, or nothing at all for a workspace
// predating workspace.Info.Agent.
func runningVersion(agent string) string {
	if agent == "" {
		return ""
	}
	return "; it runs remote-dockerd " + agent
}

// greet opens a channel and completes its version handshake. An agent too old
// for the command runs it as a shell command and says nothing, so the greeting
// is what tells them apart. hello returns false for a line that is not one.
func greet[T any](ctx context.Context, client *tunnelclient.Client, command string, frame, want int, hello func(T) (int, bool)) (io.ReadWriteCloser, *bufio.Reader, T, error) {
	var greeting T
	stream, err := client.OpenStream(command)
	if err != nil {
		return nil, nil, greeting, err
	}

	r := bufio.NewReaderSize(stream, frame)
	line, waited, err := readGreeting(ctx, stream, r)
	if err != nil {
		_ = stream.Close()
		if errors.Is(err, context.Canceled) {
			// The session closing, not a silent workspace.
			return nil, nil, greeting, err
		}
		if errors.Is(err, context.DeadlineExceeded) && waited > 0 {
			return nil, nil, greeting, &silentError{command: command, after: waited}
		}
		if errors.Is(err, io.EOF) {
			// A CLEAN end: an agent with no case for the command. A broken
			// link ends otherwise and keeps its own error.
			return nil, nil, greeting, &notServedError{command: command}
		}
		return nil, nil, greeting, fmt.Errorf("no greeting from the workspace: %w", err)
	}
	version, ok := 0, false
	if json.Unmarshal([]byte(line), &greeting) == nil {
		version, ok = hello(greeting)
	}
	if !ok {
		_ = stream.Close()
		return nil, nil, greeting, &notServedError{command: command}
	}
	if version != want {
		_ = stream.Close()
		return nil, nil, greeting, fmt.Errorf("the workspace speaks %s version %d, this client speaks %d",
			command, version, want)
	}
	return stream, r, greeting, nil
}

// readGreeting reads the greeting line, bounded; on timeout it closes the
// stream to fail the blocked read. It returns the budget actually given, which
// may be the caller's shorter deadline.
func readGreeting(ctx context.Context, stream io.Closer, r *bufio.Reader) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	// Rounded to the second: under half a second is 0, which is not "silent".
	deadline, _ := ctx.Deadline()
	budget := time.Until(deadline).Round(time.Second)

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		done <- result{line, err}
	}()

	select {
	case res := <-done:
		return res.line, budget, res.err
	case <-ctx.Done():
		_ = stream.Close()
		return "", budget, ctx.Err()
	}
}

// do sends one request, with its payload, and returns the answer. Replies are
// untagged, so the lock spans the whole exchange. A timed-out exchange CLOSES
// the channel: there is no deadline to set, and a half-read reply would leave
// the next caller reading from the middle of a tar.
func (c *cacheChannel) do(ctx context.Context, req cache.Request, body io.Reader) (cache.Reply, error) {
	if err := req.Validate(); err != nil {
		return cache.Reply{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	type answer struct {
		reply cache.Reply
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		reply, err := c.exchange(req, body)
		done <- answer{reply, err}
	}()

	select {
	case a := <-done:
		return a.reply, a.err
	case <-ctx.Done():
		_ = c.stream.Close()
		return cache.Reply{}, fmt.Errorf("cache: %s for %s did not answer: %w",
			req.Op, req.Export, ctx.Err())
	}
}

// exchange is the request and its reply, on a stream the caller has locked.
func (c *cacheChannel) exchange(req cache.Request, body io.Reader) (cache.Reply, error) {
	encoded, err := json.Marshal(req)
	if err != nil {
		return cache.Reply{}, err
	}
	if len(encoded)+1 > cache.MaxFrame {
		return cache.Reply{}, fmt.Errorf("cache: a %s request for %s is too long to send",
			req.Op, req.Export)
	}
	if _, err := c.stream.Write(append(encoded, '\n')); err != nil {
		return cache.Reply{}, fmt.Errorf("cache: sending %s: %w", req.Op, err)
	}
	if body != nil && req.Bytes > 0 {
		// Exactly the bytes promised, or the stream desynchronises.
		if _, err := io.CopyN(c.stream, body, req.Bytes); err != nil {
			return cache.Reply{}, fmt.Errorf("cache: sending the batch for %s: %w", req.Export, err)
		}
	}

	line, err := c.r.ReadString('\n')
	if err != nil {
		return cache.Reply{}, fmt.Errorf("cache: the workspace stopped answering: %w", err)
	}
	var reply cache.Reply
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		return cache.Reply{}, fmt.Errorf("cache: reading the answer for %s: %w", req.Op, err)
	}
	// A promised payload is read whatever the outcome, for the same reason.
	if reply.Bytes > 0 {
		reply.Payload = make([]byte, reply.Bytes)
		if _, err := io.ReadFull(c.r, reply.Payload); err != nil {
			return cache.Reply{}, fmt.Errorf("cache: reading the payload for %s: %w", req.Op, err)
		}
	}
	if reply.Err != "" {
		return reply, fmt.Errorf("%s", reply.Err)
	}
	return reply, nil
}

// Changes asks what the container did to a share, converting the wire type to
// dircache's field for field (ADR 0021).
func (c *cacheChannel) Changes(ctx context.Context, export string) ([]dircache.Change, error) {
	reply, err := c.do(ctx, cache.Request{Op: cache.OpChanges, Export: export}, nil)
	if reply.Unknown {
		return nil, dircache.ErrShareGone
	}
	if err != nil {
		return nil, err
	}
	changes := make([]dircache.Change, 0, len(reply.Changes))
	for _, ch := range reply.Changes {
		changes = append(changes, dircache.Change{
			Path:    ch.Path,
			Size:    ch.Size,
			ModTime: ch.ModTime,
			Deleted: ch.Deleted,
		})
	}
	return changes, nil
}

// Pull fetches the named paths in chunks, calling into once per file.
func (c *cacheChannel) Pull(ctx context.Context, export string, paths []string, into func(dircache.File) error) error {
	for _, batch := range chunkPaths(paths) {
		reply, err := c.do(ctx, cache.Request{
			Op:     cache.OpPull,
			Export: export,
			Paths:  batch,
		}, nil)
		if err != nil {
			return err
		}
		if err := untar(bytes.NewReader(reply.Payload), into); err != nil {
			return err
		}
	}
	return nil
}

// untar hands over each regular file; nothing else is safe to carry back.
func untar(body io.Reader, into func(dircache.File) error) error {
	tr := tar.NewReader(body)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if err := into(dircache.File{
			Path:    "/" + strings.TrimPrefix(header.Name, "/"),
			ModTime: header.ModTime,
			Mode:    header.FileInfo().Mode(),
			Body:    tr,
		}); err != nil {
			return err
		}
	}
}

// Mounted names the cache volumes the workspace has a union on.
func (c *cacheChannel) Mounted(ctx context.Context) ([]string, error) {
	reply, err := c.do(ctx, cache.Request{Op: cache.OpMounted}, nil)
	if err != nil {
		return nil, err
	}
	return reply.Caches, nil
}

// Prepare mounts a share's union and answers with the path a container binds.
func (c *cacheChannel) Prepare(ctx context.Context, export, volume string, port int, mode workspace.Mode) (string, error) {
	reply, err := c.do(ctx, cache.Request{
		Op:     cache.OpPrepare,
		Export: export,
		Port:   port,
		Cache:  volume,
		Read:   string(mode.Read),
	}, nil)
	if err != nil {
		return "", err
	}
	if reply.Merged == "" {
		return "", fmt.Errorf("cache: the workspace prepared %s and named no path", export)
	}
	return reply.Merged, nil
}

// Apply puts one batch of files, read from root, into a share's cache.
func (c *cacheChannel) Apply(ctx context.Context, export, root string, entries []dircache.Entry) error {
	body, err := tarOf(root, entries, c.codec)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, cache.Request{
		Op:     cache.OpApply,
		Export: export,
		Bytes:  int64(len(body)),
		Codec:  c.codec,
	}, bytes.NewReader(body))
	return err
}

// Drop removes paths from a share's cache, in as many requests as the frame
// cap needs.
func (c *cacheChannel) Drop(ctx context.Context, export string, paths []string) error {
	for _, batch := range chunkPaths(paths) {
		if _, err := c.do(ctx, cache.Request{
			Op:     cache.OpDrop,
			Export: export,
			Paths:  batch,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Close ends the channel, which releases every union this session prepared.
func (c *cacheChannel) Close() error { return c.stream.Close() }

// liveCache is this session's cache channel, or nil. Asked per batch: a fill
// outlives the connection it started on (ADR 0015).
func (s *Session) liveCache() *cacheChannel {
	live, ok := s.gate.currentLive()
	if !ok || live == nil {
		return nil
	}
	// Memoised; a mount's refusal is where the reason is said.
	c, _ := s.ensureCacheChan(s.ctx, live)
	return c
}

// liveStore is liveCache as a dircache.Store, never a typed nil in an
// interface.
func (s *Session) liveStore() (dircache.Store, bool) {
	live := s.liveCache()
	if live == nil {
		return nil, false
	}
	return live, true
}

// shareCache is what the rewriter is handed: the channel for Prepare, which
// the container waits on, and the session for the fill, which outlives the
// connection (ADR 0015).
type shareCache struct {
	*cacheChannel
	session *Session
}

// Attach hands the share to the engine, if any, translating the mode into
// dircache's terms, which cannot name workspace.Mode.
func (c shareCache) Attach(export, localPath string, mode workspace.Mode) {
	if c.session.cache != nil {
		c.session.cache.Attach(export, localPath, dircache.ShareOptions{
			Prefetch:  mode.Prefetch(),
			Ephemeral: mode.Write == workspace.WriteEphemeral,
		})
	}
}

// pathsPerFrame bounds the paths one request names: they ride in the header
// line, capped at cache.MaxFrame, and an oversized one is refused whole. Half,
// leaving room for the rest of the line and JSON escaping.
const pathsPerFrame = cache.MaxFrame / 2

// tarOf builds the batch in memory, since a payload is framed by its length.
func tarOf(root string, entries []dircache.Entry, codec string) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Path)
	}

	var buf bytes.Buffer
	if codec != cache.CodecZstd {
		// Not `return buf.Bytes(), WriteTar(...)`: that returns an empty slice.
		if err := cache.WriteTar(cache.TarFilesFrom(root, names), &buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// The frame length describes the encoded bytes.
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if err := cache.WriteTar(cache.TarFilesFrom(root, names), zw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	// Closed before the buffer is read, or the payload ends early.
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// chunkPaths splits a path list into requests that each fit one frame.
func chunkPaths(paths []string) [][]string {
	var (
		out   [][]string
		batch []string
		size  int
	)
	for _, p := range paths {
		// Quotes, a comma, and headroom for escaping.
		cost := len(p) + 8
		if len(batch) > 0 && size+cost > pathsPerFrame {
			out = append(out, batch)
			batch, size = nil, 0
		}
		batch = append(batch, p)
		size += cost
	}
	if len(batch) > 0 {
		out = append(out, batch)
	}
	return out
}
