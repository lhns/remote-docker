//go:build linux

package supervise

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Cancelling Run is how the agent shuts down, and it has to reach dockerd as a
// request to stop. exec.CommandContext's default is SIGKILL, and a killed
// daemon leaves its runtime state behind for the next start (see
// daemons.ExecRoot). Stands in a script for dockerd-entrypoint.sh that records
// being asked.
func TestCancellingRunAsksTheDaemonToStop(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "asked")
	script := "#!/bin/sh\ntrap 'echo asked > " + marker + "; exit 0' TERM INT\nwhile :; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, command), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := &Dockerd{Socket: filepath.Join(dir, "docker.sock")}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()

	// Started, and its trap installed.
	time.Sleep(500 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the daemon was killed rather than asked to stop")
	}
}
