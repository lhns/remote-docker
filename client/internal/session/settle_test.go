package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/dircache"
)

// oneFileStore is a workspace whose container wrote one file.
type oneFileStore struct {
	mu     sync.Mutex
	path   string
	body   string
	at     time.Time
	pulled int
}

func (s *oneFileStore) Apply(context.Context, string, string, []dircache.Entry) ([]string, error) {
	return nil, nil
}
func (s *oneFileStore) Drop(context.Context, string, []string) error { return nil }
func (s *oneFileStore) Changes(context.Context, string) ([]dircache.Change, error) {
	return []dircache.Change{{Path: s.path, Size: int64(len(s.body)), ModTime: s.at.UnixNano(), Changed: s.at.UnixNano()}}, nil
}
func (s *oneFileStore) Pull(_ context.Context, _ string, paths []string, into func(dircache.File) error) error {
	s.mu.Lock()
	s.pulled++
	s.mu.Unlock()
	for _, p := range paths {
		if p == s.path {
			if err := into(dircache.File{Path: p, ModTime: s.at, Mode: 0o644, Body: strings.NewReader(s.body)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// A container writes a file and the session closes at once, which is how a
// job ends: Close carries it back first, and opens the connection again for it
// when the idle sweep had released it (ADR 0044, ADR 0027). Nothing depends on
// the per-connection loop having ticked.
func TestCloseCarriesBackWhatAContainerJustWrote(t *testing.T) {
	for _, released := range []bool{false, true} {
		root := t.TempDir()
		store := &oneFileStore{path: "/out.txt", body: "written just before the close", at: time.Now().Add(time.Minute)}

		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		s := &Session{ctx: ctx, cancel: cancel}
		s.cache = &dircache.Cache{
			Store:      func() (dircache.Store, bool) { return store, true },
			Ctx:        ctx,
			ChangeTime: changeTime,
		}
		opened := 0
		s.gate = &connGate[*liveConn]{
			open: func(context.Context) (*liveConn, error) {
				opened++
				return &liveConn{}, nil
			},
			shut: func(*liveConn) {},
		}
		if !released {
			// Held, as a connection serving a container is.
			if _, done, err := s.gate.acquire(ctx); err != nil {
				t.Fatal(err)
			} else {
				done()
			}
		}
		s.cache.Attach("/m/1111111111111111", root, dircache.ShareOptions{})

		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(filepath.Join(root, "out.txt"))
		if err != nil || string(got) != store.body {
			t.Errorf("released=%v: out.txt holds %q (%v) after Close, want the container's write", released, got, err)
		}
		if opened != 1 {
			t.Errorf("released=%v: the connection was opened %d times, want once", released, opened)
		}
	}
}

// A session with nothing to carry back does not reach for the workspace when
// it closes.
func TestCloseWithNothingToCarryBackOpensNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	s := &Session{ctx: ctx, cancel: cancel}
	s.cache = &dircache.Cache{Store: func() (dircache.Store, bool) { return nil, false }, Ctx: ctx}
	s.cache.Attach("/m/2222222222222222", t.TempDir(), dircache.ShareOptions{Ephemeral: true})
	s.gate = &connGate[*liveConn]{
		open: func(context.Context) (*liveConn, error) {
			t.Error("Close opened a connection with nothing to carry back")
			return &liveConn{}, nil
		},
		shut: func(*liveConn) {},
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// A machine workspace whose connection was released has stopped, and its
// unions with it: Close does not boot it to carry back nothing (ADR 0026).
func TestCloseDoesNotBootAMachineItLetGoOf(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	s := &Session{ctx: ctx, cancel: cancel}
	s.opts.Config.Machine = &config.Machine{Name: "dev"}
	s.cache = &dircache.Cache{Store: func() (dircache.Store, bool) { return nil, false }, Ctx: ctx}
	s.cache.Attach("/m/3333333333333333", t.TempDir(), dircache.ShareOptions{})
	s.gate = &connGate[*liveConn]{
		open: func(context.Context) (*liveConn, error) {
			t.Error("Close dialled a machine it had let go of")
			return &liveConn{}, nil
		},
		shut: func(*liveConn) {},
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
