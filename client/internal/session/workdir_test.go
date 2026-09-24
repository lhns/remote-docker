package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/endpointtest"
)

// A volume outlives the client process, so the export it names must mean the
// same directory in the next one. The working directory's export used to mean
// whichever directory the NEXT client started in, so a container created from
// one project mounted another after a restart.
func TestTheWorkingDirectoryExportSurvivesAChangeOfDirectory(t *testing.T) {
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "marker"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	open := func(workDir string) *Session {
		t.Chdir(workDir)
		s, err := Open(context.Background(), Options{
			Config:   config.Config{Name: "t", Host: "workspace.invalid", User: "alice", Port: 22},
			Endpoint: endpointtest.Endpoint(t),
			Role:     Host,
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	s := open(first)
	export, _, err := shareRegistrar{registry: s.registry, shares: s.shares}.Share(first)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	next := open(second)
	defer func() { _ = next.Close() }()
	share, _, ok := next.registry.LookupOrRestore(export)
	if !ok {
		t.Fatalf("%s, the export of %s, no longer resolves", export, first)
	}
	if share.LocalPath != first {
		t.Errorf("%s was bound from %s and now serves %s", export, first, share.LocalPath)
	}
}
