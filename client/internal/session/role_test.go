package session

import (
	"context"
	"slices"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/endpointtest"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/core-client/fswatch"
)

// A Query session must not bind the local Docker endpoint.
//
// This is a regression test for a real failure, and for the platform CI never
// runs. `status` and `gc` declined the workspace's export port with some care
// and then bound the LOCAL endpoint anyway, which they never use. On Unix that
// was invisible (a bind unlinks whatever socket is there) but a Windows
// named pipe genuinely excludes, so `status` could not run at all while a
// session was running. Which is exactly when a person runs `status`.
func TestQueryDoesNotBindTheEndpoint(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)

	s, err := Open(context.Background(), Options{
		Config:   config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
		WorkDir:  t.TempDir(),
		Endpoint: endpoint,
		Role:     Query,
	})
	if err != nil {
		t.Fatalf("opening a query session: %v", err)
	}
	defer func() { _ = s.Close() }()

	if proxy.Reachable(endpoint) {
		t.Fatal("a query session is answering on the endpoint; it must leave it for the host session")
	}

	// It still REPORTS the endpoint, because commands print it and a session
	// that serves nothing should still be able to say where it would be.
	if s.Endpoint == "" {
		t.Error("a query session should still report where the endpoint is")
	}
}

// A Host session binds it, because being the endpoint is what it is for.
func TestHostBindsTheEndpoint(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)

	s, err := Open(context.Background(), Options{
		Config:   config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
		WorkDir:  t.TempDir(),
		Endpoint: endpoint,
		Role:     Host,
	})
	if err != nil {
		t.Fatalf("opening a host session: %v", err)
	}
	defer func() { _ = s.Close() }()

	if !proxy.Reachable(endpoint) {
		t.Fatal("a host session is not answering on the endpoint it bound")
	}
}

// And two host sessions for one endpoint is a conflict, not a silent takeover.
//
// A Unix bind that removes an existing socket first would let the second
// process unlink a RUNNING one's socket and take its place, leaving the first
// accepting on an inode nobody can reach (ADR 0017). Binding is not a lock.
func TestASecondHostIsRefused(t *testing.T) {
	endpoint := endpointtest.Endpoint(t)
	opts := Options{
		Config:   config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
		WorkDir:  t.TempDir(),
		Endpoint: endpoint,
		Role:     Host,
	}

	first, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("opening the first session: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := Open(context.Background(), opts)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second host session bound an endpoint another session is serving")
	}
}

// The role decides all three, and one bit is what it is: a Query session
// neither serves nor exports nor narrates.
func TestRoleIsOneBit(t *testing.T) {
	if !Host.hosting() {
		t.Error("Host does not host")
	}
	if Query.hosting() {
		t.Error("Query hosts")
	}
	if Query.String() != "query" || Host.String() != "host" {
		t.Errorf("roles print as %q and %q", Query, Host)
	}
}

// The cache excludes what the watcher excludes. fswatch substitutes its
// defaults for a nil list, and a cache handed the raw nil walks .git and
// node_modules, which the watcher then never invalidates, so a cached copy
// of them is stale for good.
func TestTheCacheExcludesWhatTheWatcherExcludes(t *testing.T) {
	s, err := Open(context.Background(), Options{
		Config:   config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
		WorkDir:  t.TempDir(),
		Endpoint: endpointtest.Endpoint(t),
		Role:     Host,
	})
	if err != nil {
		t.Fatalf("opening a host session: %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.cache == nil {
		t.Fatal("a host session has no cache")
	}
	if !slices.Equal(s.cache.Exclude, fswatch.DefaultExcludes) {
		t.Errorf("cache excludes = %v, want the watcher's defaults %v", s.cache.Exclude, fswatch.DefaultExcludes)
	}
}
