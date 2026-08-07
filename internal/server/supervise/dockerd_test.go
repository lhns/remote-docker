package supervise

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitArgs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"--storage-driver=fuse-overlayfs", []string{"--storage-driver=fuse-overlayfs"}},
		{"--debug --storage-driver=vfs", []string{"--debug", "--storage-driver=vfs"}},
		{"  --debug   --iptables=false  ", []string{"--debug", "--iptables=false"}},
	}
	for _, tt := range tests {
		if got := SplitArgs(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("SplitArgs(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// Readiness is the socket's presence, which is what the shell entrypoint
// waited on too -- and getting it wrong means the first `docker ps` after a
// login fails for no reason the user can act on.
func TestReady(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	d := &Dockerd{Socket: socket}

	if d.Ready() {
		t.Error("reported ready with no socket present")
	}

	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.Ready() {
		t.Error("reported not ready with the socket present")
	}
}

func TestWaitReady(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	d := &Dockerd{Socket: socket, StartTimeout: 5 * time.Second}

	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(socket, nil, 0o600)
	}()

	if err := d.WaitReady(t.Context()); err != nil {
		t.Errorf("WaitReady: %v", err)
	}
}

// A timeout has to be reported rather than waited on forever, and the message
// has to say what did not appear -- the caller treats it as non-fatal and logs
// it, so it is the only account anyone gets.
func TestWaitReadyTimesOut(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	d := &Dockerd{Socket: socket, StartTimeout: 100 * time.Millisecond}

	err := d.WaitReady(t.Context())
	if err == nil {
		t.Fatal("WaitReady returned success with no socket")
	}
	if !strings.Contains(err.Error(), socket) {
		t.Errorf("error %q does not name the socket that did not appear", err)
	}
}

func TestWaitReadyHonoursCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	d := &Dockerd{Socket: socket, StartTimeout: time.Hour}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := d.WaitReady(ctx); err == nil {
		t.Error("WaitReady ignored cancellation")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("WaitReady took %s to notice cancellation", elapsed)
	}
}

// Stopping something that was never started must not panic -- the serve path
// calls Stop unconditionally on the way out.
func TestStopWithNothingRunning(t *testing.T) {
	if err := (&Dockerd{}).Stop(); err != nil {
		t.Errorf("Stop with no process: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	d := &Dockerd{}
	d.applyDefaults()

	if d.Command != DefaultCommand {
		t.Errorf("Command = %q, want %q", d.Command, DefaultCommand)
	}
	if d.Socket != DefaultSocket {
		t.Errorf("Socket = %q, want %q", d.Socket, DefaultSocket)
	}
	if d.StartTimeout == 0 || d.RestartDelay == 0 {
		t.Error("timeouts were left at zero, which would spin or never wait")
	}

	// Explicit values are not overwritten.
	custom := &Dockerd{Command: "mine", Socket: "/tmp/x.sock", StartTimeout: time.Second, RestartDelay: time.Second}
	custom.applyDefaults()
	if custom.Command != "mine" || custom.Socket != "/tmp/x.sock" {
		t.Errorf("defaults overwrote explicit values: %+v", custom)
	}
}
