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
	"io/fs"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

	// owner says the workspace gives a union's root the owner a prepare names
	// (cache.Hello.Owner). An agent before it ignores the field.
	owner bool

	// carry says the workspace reports opaque directories and links, and skips
	// and names an entry of a batch it cannot write (cache.Hello.Carry). Asked
	// for in each request only when it is set, since an agent without it would
	// ignore the field and the client would take a skipped entry for a landed
	// one.
	carry bool

	// times says the workspace reports change times, modes and empty
	// directories when asked (cache.Hello.Times). Without them a deletion of a
	// file the cache never held is kept, and status says the workspace is too
	// old.
	times bool

	// dead is set when the stream can no longer be trusted: it was closed, an
	// exchange timed out, or a read or write on it failed. A request that is
	// refused by the workspace in words does not set it. The next caller opens
	// a new channel (ensureCacheChan).
	dead atomic.Bool

	// turn is held for a whole exchange (see do). A channel rather than a
	// mutex so that waiting for it honours the caller's context.
	turn chan struct{}
	r    *bufio.Reader

	// reprepare gives the workspace back a union it still serves and has no
	// record of (Session.reprepare). Nil leaves an unknown share unknown.
	reprepare func(ctx context.Context, export string) error

	// flights single-flights reprepare per share, so the write-back and the
	// fill meeting one restart ask once.
	flightMu sync.Mutex
	flights  map[string]*flight
}

// flight is one reprepare in progress, which a second caller waits for.
type flight struct {
	done chan struct{}
	err  error
}

func newCacheChannel(stream io.ReadWriteCloser, r *bufio.Reader) *cacheChannel {
	return &cacheChannel{stream: stream, r: r, turn: make(chan struct{}, 1)}
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

	c := newCacheChannel(stream, r)
	// Chosen from what the AGENT announced, never from what this client can
	// produce: an older workspace announces none and refuses one.
	if reply.Hello.Accepts(cache.CodecZstd) {
		c.codec = cache.CodecZstd
	}
	c.owner = reply.Hello.Owner
	c.carry = reply.Hello.Carry
	c.times = reply.Hello.Times
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
	if _, ok := errors.AsType[*notServedError](err); ok {
		return fmt.Errorf("this workspace does not serve it%s%s", runningVersion(agent), rewrite.FixUpdateWorkspace)
	}
	if silent, ok := errors.AsType[*silentError](err); ok {
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

	select {
	case c.turn <- struct{}{}:
	case <-ctx.Done():
		return cache.Reply{}, fmt.Errorf("cache: %s for %s waited for the channel: %w",
			req.Op, req.Export, ctx.Err())
	}
	defer func() { <-c.turn }()

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
		if a.err != nil && !errors.Is(a.err, errRefused) {
			c.dead.Store(true)
		}
		return a.reply, a.err
	case <-ctx.Done():
		c.dead.Store(true)
		_ = c.stream.Close()
		return cache.Reply{}, fmt.Errorf("cache: %s for %s did not answer: %w",
			req.Op, req.Export, ctx.Err())
	}
}

// errRefused marks an error that is the workspace's answer in words, over a
// stream that is intact, as against one that says the stream is not.
var errRefused = errors.New("the workspace refused the request")

// refusal is the workspace's own message, kept as it came.
type refusal string

func (r refusal) Error() string        { return string(r) }
func (r refusal) Is(target error) bool { return target == errRefused }

// usable reports whether the channel can still carry a request.
func (c *cacheChannel) usable() bool { return !c.dead.Load() }

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
		return reply, refusal(reply.Err)
	}
	return reply, nil
}

// Changes asks what the container did to a share, converting the wire type to
// dircache's field for field (ADR 0021).
func (c *cacheChannel) Changes(ctx context.Context, export string) ([]dircache.Change, error) {
	reply, err := c.forShare(ctx, export, func() (cache.Reply, error) {
		return c.do(ctx, cache.Request{Op: cache.OpChanges, Export: export, Carry: c.carry, Times: c.times}, nil)
	})
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
			Opaque:  ch.Opaque,
			Link:    ch.Link,
			Changed: ch.Changed,
			Dir:     ch.Dir,
			Mode:    fs.FileMode(ch.Mode).Perm(),
		})
	}
	return changes, nil
}

// Pull fetches the named paths in chunks, calling into once per file.
func (c *cacheChannel) Pull(ctx context.Context, export string, paths []string, into func(dircache.File) error) error {
	req := cache.Request{Op: cache.OpPull, Export: export, Carry: c.carry}
	return c.perChunk(ctx, req, paths, func(reply cache.Reply) error {
		if err := untar(bytes.NewReader(reply.Payload), into); err != nil {
			return err
		}
		// What the workspace could not read is named rather than failing the
		// chunk, and handed on so write-back sets it aside until it changes.
		for _, p := range reply.Skipped {
			if err := into(dircache.File{Path: p, Unreadable: "the workspace could not read it; its log says why"}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Present answers which of the paths exist in the merged view of a share, the
// one a container sees. An agent without Carry cannot answer.
func (c *cacheChannel) Present(ctx context.Context, export string, paths []string) ([]string, error) {
	if !c.carry {
		return nil, dircache.ErrCannotPresent
	}
	var here []string
	req := cache.Request{Op: cache.OpPresent, Export: export, Carry: true}
	err := c.perChunk(ctx, req, paths, func(reply cache.Reply) error {
		here = append(here, reply.Present...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return here, nil
}

// perChunk sends req once for each chunk of paths that fits a frame, through
// forShare, and hands each reply to each.
func (c *cacheChannel) perChunk(ctx context.Context, req cache.Request, paths []string, each func(cache.Reply) error) error {
	for _, batch := range chunkPaths(paths) {
		req.Paths = batch
		reply, err := c.forShare(ctx, req.Export, func() (cache.Reply, error) { return c.do(ctx, req, nil) })
		if err != nil {
			return err
		}
		if err := each(reply); err != nil {
			return err
		}
	}
	return nil
}

// forShare runs one request about a share, and runs it once more if the
// workspace answered that it holds no union for the share and preparing it
// again worked. An agent restarted under a running container has no record of
// the union that container still binds (ADR 0044). If the union cannot be given
// back, the first answer stands; if asking failed on the way, that error is
// returned for the caller to retry.
func (c *cacheChannel) forShare(ctx context.Context, export string, op func() (cache.Reply, error)) (cache.Reply, error) {
	reply, err := op()
	if !reply.Unknown || c.reprepare == nil {
		return reply, err
	}
	if rerr := c.prepareAgain(ctx, export); rerr != nil {
		if errors.Is(rerr, errNoUnion) {
			return reply, err
		}
		return cache.Reply{}, rerr
	}
	return op()
}

// prepareAgain is reprepare, once per share at a time: a second caller waits
// for the first one's answer rather than asking again.
func (c *cacheChannel) prepareAgain(ctx context.Context, export string) error {
	c.flightMu.Lock()
	if f, ok := c.flights[export]; ok {
		c.flightMu.Unlock()
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	if c.flights == nil {
		c.flights = map[string]*flight{}
	}
	c.flights[export] = f
	c.flightMu.Unlock()

	f.err = c.reprepare(ctx, export)

	c.flightMu.Lock()
	delete(c.flights, export)
	c.flightMu.Unlock()
	close(f.done)
	return f.err
}

// errNoUnion is a share the workspace has no union for and none to give back,
// typically one released because no container held it. The caller stops
// asking about it.
var errNoUnion = errors.New("the workspace serves no union for this share")

// reprepare gives the workspace back a union it still has mounted and has no
// record of, by preparing the share as the mount did. The agent ADOPTS a union
// that is serving rather than mounting over it (ADR 0044).
//
// Only for a union the workspace's FILESYSTEM still has mounted (OpMounted):
// one released because nothing held it is gone, and preparing it again would
// mount a union for nobody, to be released and prepared again on every
// connection.
func (s *Session) reprepare(ctx context.Context, l *liveConn, c *cacheChannel, export string) error {
	share, _, ok := s.registry.Lookup(export)
	if !ok || !share.Mode.Union() {
		return errNoUnion
	}
	volume, err := workspace.CacheVolumeForExport(l.clientID, export)
	if err != nil {
		return errNoUnion
	}
	names, err := c.Mounted(ctx)
	if errors.Is(err, errRefused) {
		return fmt.Errorf("%w: %w", errNoUnion, err)
	}
	if err != nil {
		return err
	}
	if !slices.Contains(names, volume) {
		return errNoUnion
	}
	// What the mount itself would have refused (rewrite.Rewriter.union).
	owner := share.Words().Owner
	if !c.owner && owner.Set && owner != (workspace.Owner{Set: true}) {
		return errNoUnion
	}
	if _, err := c.Prepare(ctx, export, volume, l.info.NFSPort, share.Mode, owner); err != nil {
		if errors.Is(err, errRefused) {
			return fmt.Errorf("%w: %w", errNoUnion, err)
		}
		return err
	}
	s.log().Info("prepared again a union the workspace had lost track of; its container still binds it",
		"path", share.LocalPath, "export", export)
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

// ServesOwner reports whether the workspace gives a union's root the owner a
// prepare names.
func (c *cacheChannel) ServesOwner() bool { return c.owner }

// Prepare mounts a share's union and answers with the path a container binds.
func (c *cacheChannel) Prepare(ctx context.Context, export, volume string, port int, mode workspace.Mode, owner workspace.Owner) (string, error) {
	req := cache.Request{
		Op:     cache.OpPrepare,
		Export: export,
		Port:   port,
		Cache:  volume,
		Read:   string(mode.Read),
	}
	// Sent only when it is not root, which is what an agent without the field
	// makes the root anyway.
	if owner.Set && owner != (workspace.Owner{Set: true}) {
		req.Owner = owner.String()
	}
	reply, err := c.do(ctx, req, nil)
	if err != nil {
		return "", err
	}
	if reply.Merged == "" {
		return "", fmt.Errorf("cache: the workspace prepared %s and named no path", export)
	}
	return reply.Merged, nil
}

// Apply puts one batch of files, read from root, into a share's cache, and
// names the paths the workspace could not take and left out. An agent that does
// not skip fails the batch instead, so there is nothing to name then.
func (c *cacheChannel) Apply(ctx context.Context, export, root string, entries []dircache.Entry) ([]string, error) {
	body, err := tarOf(root, entries, c.codec)
	if err != nil {
		return nil, err
	}
	reply, err := c.forShare(ctx, export, func() (cache.Reply, error) {
		return c.do(ctx, cache.Request{
			Op:     cache.OpApply,
			Export: export,
			Bytes:  int64(len(body)),
			Codec:  c.codec,
			Carry:  c.carry,
		}, bytes.NewReader(body))
	})
	if err != nil {
		return nil, err
	}
	return reply.Skipped, nil
}

// Drop removes paths from a share's cache, in as many requests as the frame
// cap needs.
func (c *cacheChannel) Drop(ctx context.Context, export string, paths []string) error {
	req := cache.Request{Op: cache.OpDrop, Export: export}
	return c.perChunk(ctx, req, paths, func(cache.Reply) error { return nil })
}

// Close ends the channel, which releases every union this session prepared.
func (c *cacheChannel) Close() error {
	c.dead.Store(true)
	return c.stream.Close()
}

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
func (c shareCache) Attach(export, localPath string, mode workspace.Mode, words workspace.ShareWords) {
	c.session.attachShare(export, localPath, mode, words)
}

// attachShare hands a share to the cache engine, if there is one. A mount calls
// it for a share it registered, and a share restored from the record calls it
// too: the restored share registers for NFS and nothing else would tell the
// cache, so write-back, which polls only attached shares, would never run for it
// again (ADR 0027).
func (s *Session) attachShare(export, localPath string, mode workspace.Mode, words workspace.ShareWords) {
	if s.cache == nil {
		return
	}
	policy := policyOf(words.Prefetch)
	if !mode.Prefetch() {
		policy = dircache.PolicyOff
	}
	s.cache.Attach(export, localPath, dircache.ShareOptions{
		Policy:    policy,
		Ephemeral: mode.Write == workspace.WriteEphemeral,
		Exec:      execBits(runtime.GOOS, words.FMask, s.opts.Config.ShareFileMask),
	})
}

// execBits is the execute bits a share's container sees as this machine's
// file has them (dircache.ShareOptions.Exec): the share reports a file's
// execute bit and every other bit wide, less its file mask (ADR 0046). None on
// Windows, whose share reports every file executable.
func execBits(goos string, mask workspace.Mask, session fs.FileMode) fs.FileMode {
	if goos == "windows" {
		return 0
	}
	if !mask.Set {
		mask.Bits = session
	}
	return 0o111 &^ mask.Bits
}

// policyOf is the fill policy a share's prefetch word stands for.
func policyOf(p workspace.Prefetch) dircache.Policy {
	switch p {
	case workspace.PrefetchEager:
		return dircache.PolicyEager
	case workspace.PrefetchTree:
		return dircache.PolicyTree
	}
	return dircache.PolicyOff
}

// prefetchWord is the share word a session's default policy stands for.
func prefetchWord(p dircache.Policy) workspace.Prefetch {
	switch p {
	case dircache.PolicyEager:
		return workspace.PrefetchEager
	case dircache.PolicyTree:
		return workspace.PrefetchTree
	}
	return workspace.PrefetchOff
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
