package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/lhns/remote-docker/core-client/fswatch"
	"github.com/lhns/remote-docker/core-client/nfsserve"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/tunnel"
	"github.com/lhns/remote-docker/core/workspace"
)

// notifySink writes change frames to the agent over the workspace-notify
// channel.
type notifySink struct {
	stream io.ReadWriteCloser

	mu sync.Mutex
	w  *bufio.Writer
}

// openNotify establishes the change-notification channel and completes the
// version handshake.
//
// The handshake is not optional. The agent dispatches session commands on
// exact strings and falls through to running whatever it does not recognise,
// so an agent too old for this one runs `sh -c "workspace-notify"` and exits
// 127, indistinguishable from a working channel that has nothing to say.
// Reading a greeting first is the only thing that tells them apart.
func openNotify(client *tunnelclient.Client) (*notifySink, error) {
	stream, err := client.OpenStream(tunnel.NotifyCommand)
	if err != nil {
		return nil, err
	}

	reader := bufio.NewReader(stream)
	line, err := reader.ReadString('\n')
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("no greeting from the workspace: %w", err)
	}

	var frame workspace.NotifyFrame
	if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.Hello == nil {
		_ = stream.Close()
		return nil, fmt.Errorf("the workspace did not answer %q", tunnel.NotifyCommand)
	}
	if frame.Hello.Version != workspace.NotifyVersion {
		_ = stream.Close()
		return nil, fmt.Errorf("the workspace speaks change-notification version %d, this client speaks %d",
			frame.Hello.Version, workspace.NotifyVersion)
	}

	return &notifySink{stream: stream, w: bufio.NewWriter(stream)}, nil
}

// Send writes one frame as a single line.
func (s *notifySink) Send(_ context.Context, frame workspace.NotifyFrame) error {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(encoded)+1 > workspace.MaxNotifyFrame {
		// The far side's scanner would truncate at its buffer limit and then
		// desynchronise on the remainder, which is a far worse failure than
		// admitting one oversized frame was not sent.
		return fmt.Errorf("change frame of %d bytes exceeds the %d byte limit",
			len(encoded)+1, workspace.MaxNotifyFrame)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *notifySink) Close() error { return s.stream.Close() }

// startNotify attaches the watcher to a freshly established connection.
//
// A workspace that cannot do this is not an error worth failing a session
// over: everything else about the connection works, and the user is told once
// rather than on every reconnect.
func (s *Session) startNotify(live *liveConn) {
	if s.watch == nil {
		return
	}
	sink, err := openNotify(live.ssh)
	if err != nil {
		s.notifyOnce.Do(func() {
			s.log().Warn("file watchers inside containers will not see your edits (see ADR 0014)", "err", err)
		})
		return
	}
	live.notify = sink
	s.watch.SetSink(sink)
	s.watch.Sync(sharesOf(s.registry))
}

// sharesOf adapts the NFS registry's view of what is exported to the
// watcher's.
func sharesOf(registry *nfsserve.Registry) []fswatch.Share {
	all := registry.Shares()
	out := make([]fswatch.Share, 0, len(all))
	for _, share := range all {
		out = append(out, fswatch.Share{
			ExportPath: share.ExportPath,
			LocalPath:  share.LocalPath,
			File:       share.File,
		})
	}
	return out
}

// reconcileShares keeps the watcher's idea of what to watch in step with the
// registry.
//
// Registration already notifies directly, so this is the same belt-and-braces
// the port manager uses: a periodic pass costs nothing and covers the paths
// that do not go through the notifying one, such as the working directory,
// registered inside Open before any of this exists.
func (s *Session) reconcileShares(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.watch.Sync(sharesOf(s.registry))
		}
	}
}
