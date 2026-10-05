package main

import (
	"context"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/endpointtest"
	"github.com/lhns/remote-docker/client/internal/session"
)

// `remote status` and `remote gc` take the run of the session serving the
// endpoint (ADR 0050). Without it, on an ephemeral account, gc is a run of its
// own and collects none of that session's volumes, and status allocates a port.
func TestRunOfAsksTheBackgroundSession(t *testing.T) {
	const run = "00112233445566778899aabbccddeeff"
	endpoint := endpointtest.Endpoint(t)

	if got := runOf(endpoint); got != "" {
		t.Fatalf("with nothing serving, runOf = %q, want none", got)
	}

	s, err := session.Open(context.Background(), session.Options{
		Config:   config.Config{Host: "workspace.invalid", User: "alice", Port: 22},
		Endpoint: endpoint,
		Role:     session.Host,
		RunID:    run,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if got := runOf(endpoint); got != run {
		t.Errorf("runOf = %q, want the background session's %q", got, run)
	}
}
