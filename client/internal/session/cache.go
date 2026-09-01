package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/tunnel"
	"github.com/lhns/remote-docker/core/workspace"
)

// The client's end of the cache channel (ADR 0044).
//
// One connection per session, shared by every delegated share, and serialised:
// each request is answered before the next is written. The agent reads them the
// same way, and neither end is pipelined on purpose -- every op changes a mount
// or a file, and a protocol that could reorder them would have to say what two
// overlapping applies to one share mean.

// cacheChannel is the session's link to the workspace's union mounts.
type cacheChannel struct {
	stream io.ReadWriteCloser

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
	stream, err := client.OpenStream(tunnel.CacheCommand)
	if err != nil {
		return nil, err
	}

	c := &cacheChannel{stream: stream, r: bufio.NewReaderSize(stream, workspace.MaxCacheFrame)}

	line, err := c.r.ReadString('\n')
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("no greeting from the workspace: %w", err)
	}
	var reply workspace.CacheReply
	if err := json.Unmarshal([]byte(line), &reply); err != nil || reply.Hello == nil {
		_ = stream.Close()
		return nil, fmt.Errorf("this workspace does not serve delegated shares as a cache\n" +
			"\tfix: update the workspace, or use the cached consistency")
	}
	if reply.Hello.Version != workspace.CacheVersion {
		_ = stream.Close()
		return nil, fmt.Errorf("the workspace speaks cache version %d, this client speaks %d",
			reply.Hello.Version, workspace.CacheVersion)
	}
	return c, nil
}

// do sends one request, with its payload if it has one, and returns the answer.
//
// The lock spans the whole exchange rather than the write alone: the replies
// are not tagged, so a second request written before the first was answered
// would make every reply after it belong to the wrong caller.
func (c *cacheChannel) do(req workspace.CacheRequest, body io.Reader) (workspace.CacheReply, error) {
	if err := req.Validate(); err != nil {
		return workspace.CacheReply{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	encoded, err := json.Marshal(req)
	if err != nil {
		return workspace.CacheReply{}, err
	}
	if len(encoded)+1 > workspace.MaxCacheFrame {
		return workspace.CacheReply{}, fmt.Errorf("cache: a %s request for %s is too long to send",
			req.Op, req.Export)
	}
	if _, err := c.stream.Write(append(encoded, '\n')); err != nil {
		return workspace.CacheReply{}, fmt.Errorf("cache: sending %s: %w", req.Op, err)
	}
	if body != nil && req.Bytes > 0 {
		// Exactly the number of bytes the header promised. A short body would
		// leave the agent reading the next request out of the middle of a tar.
		if _, err := io.CopyN(c.stream, body, req.Bytes); err != nil {
			return workspace.CacheReply{}, fmt.Errorf("cache: sending the batch for %s: %w", req.Export, err)
		}
	}

	line, err := c.r.ReadString('\n')
	if err != nil {
		return workspace.CacheReply{}, fmt.Errorf("cache: the workspace stopped answering: %w", err)
	}
	var reply workspace.CacheReply
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		return workspace.CacheReply{}, fmt.Errorf("cache: reading the answer for %s: %w", req.Op, err)
	}
	// Whatever the outcome, a promised payload is read: it follows on the same
	// stream, and leaving it there would put the next reply in the middle of a
	// tar.
	if reply.Bytes > 0 {
		reply.Payload = make([]byte, reply.Bytes)
		if _, err := io.ReadFull(c.r, reply.Payload); err != nil {
			return workspace.CacheReply{}, fmt.Errorf("cache: reading the payload for %s: %w", req.Op, err)
		}
	}
	if reply.Err != "" {
		return reply, fmt.Errorf("%s", reply.Err)
	}
	return reply, nil
}

// Changes asks what the container did to a share.
func (c *cacheChannel) Changes(_ context.Context, export string) ([]workspace.CacheChange, error) {
	reply, err := c.do(workspace.CacheRequest{Op: workspace.OpChanges, Export: export}, nil)
	if err != nil {
		return nil, err
	}
	return reply.Changes, nil
}

// Pull fetches the named paths out of a share's cache layer, as a tar.
func (c *cacheChannel) Pull(_ context.Context, export string, paths []string) ([]byte, error) {
	reply, err := c.do(workspace.CacheRequest{
		Op:     workspace.OpPull,
		Export: export,
		Paths:  paths,
	}, nil)
	if err != nil {
		return nil, err
	}
	return reply.Payload, nil
}

// Prepare mounts a share's union and answers with the path a container binds.
func (c *cacheChannel) Prepare(_ context.Context, export, cache string, port int) (string, error) {
	reply, err := c.do(workspace.CacheRequest{
		Op:     workspace.OpPrepare,
		Export: export,
		Port:   port,
		Cache:  cache,
	}, nil)
	if err != nil {
		return "", err
	}
	if reply.Merged == "" {
		return "", fmt.Errorf("cache: the workspace prepared %s and named no path", export)
	}
	return reply.Merged, nil
}

// Apply writes a tar into a share's cache.
func (c *cacheChannel) Apply(_ context.Context, export string, size int64, body io.Reader) error {
	_, err := c.do(workspace.CacheRequest{
		Op:     workspace.OpApply,
		Export: export,
		Bytes:  size,
	}, body)
	return err
}

// Drop removes paths from a share's cache, which is what a deletion here
// becomes.
func (c *cacheChannel) Drop(_ context.Context, export string, paths []string) error {
	_, err := c.do(workspace.CacheRequest{
		Op:     workspace.OpDrop,
		Export: export,
		Paths:  paths,
	}, nil)
	return err
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

func (c shareCache) Fill(export, localPath string) { c.session.Fill(export, localPath) }

// pathsPerFrame bounds how many paths one request names.
//
// The paths of a pull or a drop ride in the JSON header line, which the
// protocol caps at workspace.MaxCacheFrame -- so a `git checkout` across a
// large branch, or a build that wrote ten thousand files, is not a big request
// but a REFUSED one, and the refusal is the whole operation rather than a part
// of it. Half the frame, because the op, the export and JSON's own escaping
// share the line.
const pathsPerFrame = workspace.MaxCacheFrame / 2

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
