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

	"github.com/klauspost/compress/zstd"

	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/cache"
	"github.com/lhns/remote-docker/dircache"
)

// The client's end of the cache channel (ADR 0044), which is this project's
// dircache.Store: the wire format, the tar and its codec are all in here, and
// none of them is visible to the policy that drives it.
//
// One connection per session, shared by every delegated share, and serialised:
// each request is answered before the next is written, as the agent reads them
// (agent/internal/sshd/cache.go, which has why).

// cacheChannel is the session's link to the workspace's union mounts.
type cacheChannel struct {
	stream io.ReadWriteCloser

	// codec is the payload encoding this workspace said it can read, settled
	// once in the greeting and fixed for the life of the channel.
	codec string

	mu sync.Mutex
	r  *bufio.Reader
}

// openCache establishes the channel and completes the version handshake.
//
// The handshake is what tells an agent too old for this command from a working
// one: the agent dispatches on exact strings and runs anything else, so an old
// one runs `sh -c "workspace-cache"` and exits 127 with nothing to say. Reading
// a greeting first is the only thing that separates them.
func openCache(client *tunnelclient.Client) (*cacheChannel, error) {
	stream, err := client.OpenStream(cache.Command)
	if err != nil {
		return nil, err
	}

	c := &cacheChannel{stream: stream, r: bufio.NewReaderSize(stream, cache.MaxFrame)}

	line, err := c.r.ReadString('\n')
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("no greeting from the workspace: %w", err)
	}
	var reply cache.Reply
	if err := json.Unmarshal([]byte(line), &reply); err != nil || reply.Hello == nil {
		_ = stream.Close()
		return nil, fmt.Errorf("this workspace does not serve delegated shares as a cache\n" +
			"\tfix: update the workspace, or use the cached consistency")
	}
	if reply.Hello.Version != cache.Version {
		_ = stream.Close()
		return nil, fmt.Errorf("the workspace speaks cache version %d, this client speaks %d",
			reply.Hello.Version, cache.Version)
	}
	// Chosen from what the AGENT said it can read, never from what this client
	// can produce: a workspace older than compression announces no codecs at
	// all, and sending it one would be refused rather than negotiated.
	if reply.Hello.Accepts(cache.CodecZstd) {
		c.codec = cache.CodecZstd
	}
	return c, nil
}

// do sends one request, with its payload if it has one, and returns the answer.
//
// The lock spans the whole exchange rather than the write alone: the replies
// are not tagged, so a second request written before the first was answered
// would make every reply after it belong to the wrong caller.
//
// The context bounds it, and the only honest way to end a timed-out exchange is
// to CLOSE the channel. It is an SSH stream with no deadline to set, and a reply
// abandoned half read leaves the next caller reading a JSON line out of the
// middle of a tar. A wedged workspace therefore costs this channel, which the
// session opens again on the next connection, rather than costing every request
// after it.
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
		// Exactly the number of bytes the header promised. A short body would
		// leave the agent reading the next request out of the middle of a tar.
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
	// Whatever the outcome, a promised payload is read: it follows on the same
	// stream, and leaving it there would put the next reply in the middle of a
	// tar.
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

// Changes asks what the container did to a share.
func (c *cacheChannel) Changes(ctx context.Context, export string) ([]cache.Change, error) {
	reply, err := c.do(ctx, cache.Request{Op: cache.OpChanges, Export: export}, nil)
	if reply.Unknown {
		return nil, dircache.ErrShareGone
	}
	if err != nil {
		return nil, err
	}
	return reply.Changes, nil
}

// Pull fetches the named paths, calling into once per file.
//
// Chunked for the same reason Drop is, and unpacked here because the tar is
// this channel's own encoding: it built the one going the other way.
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

// untar hands each regular file in an archive over.
//
// Only regular files: a written-back directory is made by the writer as it
// needs one, and nothing else in a cache layer can be carried to another
// machine safely.
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
func (c *cacheChannel) Prepare(ctx context.Context, export, volume string, port int) (string, error) {
	reply, err := c.do(ctx, cache.Request{
		Op:     cache.OpPrepare,
		Export: export,
		Port:   port,
		Cache:  volume,
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
//
// Entries rather than bytes, so the codec stays in here: the frame's length has
// to describe what is ACTUALLY sent, which means whatever builds the tar has to
// know how it was encoded. Handing the caller that fact made it the caller's
// problem in two files.
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

// Drop removes paths from a share's cache, which is what a deletion here
// becomes.
//
// However many: the paths ride in the JSON header line, which the protocol
// caps, so a `git checkout` across a large branch is several requests. That is
// a fact about the wire and no caller has to know it.
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

// liveCache is this session's cache channel, or nil when there is no live
// connection or the workspace does not serve one.
//
// Asked per batch rather than captured once: a fill outlives the request that
// started it, and the connection under it can be released and reopened while it
// runs (ADR 0015).
func (s *Session) liveCache() *cacheChannel {
	live, ok := s.gate.currentLive()
	if !ok || live == nil {
		return nil
	}
	s.shareCacheFor(live)
	return live.cacheChan
}

// liveStore is what dircache is given, and the nil check is why it is not
// liveCache itself: a typed nil pointer in an interface is not a nil interface,
// so handing one over would make every call a panic instead of a no-op.
func (s *Session) liveStore() (dircache.Store, bool) {
	live := s.liveCache()
	if live == nil {
		return nil, false
	}
	return live, true
}

// shareCache is what the rewriter is handed: the channel for the request the
// container is waiting on, and the session for the fill it is not.
//
// Two objects because the two have different lifetimes. Prepare must finish
// before the container is created; the fill outlives the request entirely and
// belongs to the session, which is what survives a connection being released
// and reopened underneath it (ADR 0015).
type shareCache struct {
	*cacheChannel
	session *Session
}

// Fill starts the background fill, and does nothing on a session that caches
// nothing. The engine is a pointer where the state it replaced was a value, so
// this is a check rather than the no-op it used to be for free.
func (c shareCache) Fill(export, localPath string) {
	if c.session.cache != nil {
		c.session.cache.Fill(export, localPath)
	}
}

// pathsPerFrame bounds how many paths one request names.
//
// The paths of a pull or a drop ride in the JSON header line, which the
// protocol caps at cache.MaxFrame -- so a `git checkout` across a
// large branch, or a build that wrote ten thousand files, is not a big request
// but a REFUSED one, and the refusal is the whole operation rather than a part
// of it. Half the frame, because the op, the export and JSON's own escaping
// share the line.
const pathsPerFrame = cache.MaxFrame / 2

// tarOf builds the batch.
//
// In memory because the channel frames a payload by length: the workspace has
// to be told how many bytes follow before they are sent. dircache bounds a
// batch before it gets here.
func tarOf(root string, entries []dircache.Entry, codec string) ([]byte, error) {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Path)
	}

	var buf bytes.Buffer
	if codec != cache.CodecZstd {
		// Written before the buffer is read: `return buf.Bytes(), WriteTar(...)`
		// evaluates the bytes first and hands back an empty slice.
		if err := cache.WriteTar(cache.TarFilesFrom(root, names), &buf); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	// The compressor wraps the tar writer, so the tar is written once and the
	// bytes that leave are the encoded ones, which is what the frame's length
	// has to describe. Default level: a source tree compresses hard enough that
	// the link, not the CPU, is what the fill waits on.
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if err := cache.WriteTar(cache.TarFilesFrom(root, names), zw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	// Closed before the buffer is measured, or the payload's length is right
	// and its contents end early.
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
		// Quotes, a comma, and headroom for the escaping of a name this does
		// not inspect.
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
