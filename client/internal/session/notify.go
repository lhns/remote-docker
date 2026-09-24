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
func openNotify(ctx context.Context, client *tunnelclient.Client) (*notifySink, error) {
	stream, _, _, err := greet(ctx, client, notify.Command, notify.MaxFrame, notify.Version,
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
		// The agent's scanner would truncate it and desynchronise.
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

// startNotify attaches the watcher to a new connection. A workspace without
// the channel is warned about once, not failed.
func (s *Session) startNotify(live *liveConn) {
	if s.watch == nil {
		return
	}
	sink, err := openNotify(s.ctx, live.ssh)
	if err != nil {
		s.notifyOnce.Do(func() {
			s.log().Warn("file watchers inside containers will not see your edits (see ADR 0014)", "err", err)
		})
		return
	}
	live.notify = sink
	s.watch.SetSink(sink)
	s.syncWatch()
}

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

// reconcileShares periodically resyncs the watcher, covering shares registered
// without notifying it, such as a share restored from the record.
func (s *Session) reconcileShares(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Syncing while dormant would restore the watches Standby dropped.
			if s.isDormant() {
				continue
			}
			s.syncWatch()
		}
	}
}
