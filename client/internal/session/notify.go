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
	"github.com/lhns/remote-docker/core/notify"
)

// notifySink writes change frames to the agent over the workspace-notify
// channel.
type notifySink struct {
	stream io.ReadWriteCloser

	mu sync.Mutex
	w  *bufio.Writer
}

// openNotify establishes the change-notification channel and completes the
// version handshake (see greet).
func openNotify(client *tunnelclient.Client) (*notifySink, error) {
	stream, _, _, err := greet(client, notify.Command, notify.MaxFrame, notify.Version,
		func(frame notify.Frame) (int, bool) {
			if frame.Hello == nil {
				return 0, false
			}
			return frame.Hello.Version, true
		})
	if err != nil {
		return nil, err
	}
	return &notifySink{stream: stream, w: bufio.NewWriter(stream)}, nil
}

// Send writes one frame as a single line.
func (s *notifySink) Send(_ context.Context, frame notify.Frame) error {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if len(encoded)+1 > notify.MaxFrame {
		// The far side's scanner would truncate at its buffer limit and then
		// desynchronise on the remainder, which is a far worse failure than
		// admitting one oversized frame was not sent.
		return fmt.Errorf("change frame of %d bytes exceeds the %d byte limit",
			len(encoded)+1, notify.MaxFrame)
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
			// Nothing to reconcile while dormant, and re-syncing the real set
			// would put back the watches Standby just dropped.
			if s.isDormant() {
				continue
			}
			s.watch.Sync(sharesOf(s.registry))
		}
	}
}
