package unions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core-agent/union"
)

// helperEnv makes the test binary act as a union child; see TestHelperChild.
const (
	helperEnv = "UNIONS_TEST_CHILD"
	markerEnv = "UNIONS_TEST_MARKER"
)

// TestHelperChild is the process childManager starts: "fail" prints a message
// and exits 1, "serve" stays up until killed. A no-op in an ordinary run.
func TestHelperChild(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "fail":
		// The shape of a failed child: output of its own, then main's
		// "remote-dockerd: " line and its fix.
		fmt.Fprintln(os.Stderr, "fuse: chatter before the error")
		fmt.Fprintln(os.Stderr, "remote-dockerd: fuse-overlayfs is not in the image this daemon runs\n  fix: set WORKSPACE_DIND_IMAGE")
		os.Exit(1)
	case "fail-once":
		// The first attempt leaves a marker and fails; the next one says it
		// is serving and stays up.
		marker := os.Getenv(markerEnv)
		if _, err := os.Stat(marker); err != nil {
			_ = os.WriteFile(marker, nil, 0o600)
			fmt.Fprintln(os.Stderr, "mount: connection refused")
			os.Exit(1)
		}
		_ = os.WriteFile(marker+".served", nil, 0o600)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "serve":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func childManager(t *testing.T, behaviour string) *Manager {
	t.Helper()
	m := manager(t)
	marker := filepath.Join(t.TempDir(), "attempted")
	m.probe = func(context.Context, union.Spec) error {
		if _, err := os.Stat(marker + ".served"); err != nil {
			return errors.New("not mounted")
		}
		return nil
	}
	m.command = func(ctx context.Context, _ union.Spec) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperChild$")
		cmd.Env = append(os.Environ(), helperEnv+"="+behaviour, markerEnv+"="+marker)
		return cmd
	}
	return m
}

// stop ends the supervisor Prepare started.
func stop(m *Manager, account, export string) {
	k := key(account, thisClient, export)
	m.mu.Lock()
	l := m.shares[k]
	m.mu.Unlock()
	if l != nil {
		m.discard(k, l)
	}
}

// A child that exits before the mount answers ends Prepare at once, carrying
// the child's own words rather than a timeout that names no cause.
func TestPrepareReportsAChildThatExits(t *testing.T) {
	m := childManager(t, "fail")
	m.probe = func(context.Context, union.Spec) error { return errors.New("not mounted") }

	begun := time.Now()
	_, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, prepareFor(t, coldExport))
	if err == nil {
		t.Fatal("Prepare succeeded with a child that exited")
	}
	if took := time.Since(begun); took > 10*time.Second {
		t.Errorf("Prepare took %s; it should end when the child does", took)
	}
	for _, want := range []string{"fuse-overlayfs is not in the image", "\n  fix: set WORKSPACE_DIND_IMAGE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	for _, unwanted := range []string{"remote-dockerd", "chatter", "unions:"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("error %q still carries %q", err, unwanted)
		}
	}
}

// The reported error is the child's last one, whatever the program it ran said
// after it, and a child with no error line is reported by its last line.
func TestTailBufferReportsTheLastError(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"with a fix": {"noise\nremote-dockerd: bad thing\n  fix: do this\n", "bad thing\n  fix: do this"},
		"last wins":  {"remote-dockerd: first\nremote-dockerd: second\n", "second"},
		"no prefix":  {"one\ntwo\n", "two"},
		"empty":      {"", ""},
	} {
		tail := &tailBuffer{max: tailBytes}
		_, _ = tail.Write([]byte(tc.in))
		if got := tail.report(); got != tc.want {
			t.Errorf("%s: report = %q, want %q", name, got, tc.want)
		}
	}
	// A chatty child cannot push the error out of the kept tail.
	tail := &tailBuffer{max: 64}
	_, _ = tail.Write([]byte("remote-dockerd: the error\n  fix: do this\n" + strings.Repeat("x\n", 500)))
	if got := tail.report(); got != "the error\n  fix: do this" {
		t.Errorf("after a chatty child, report = %q", got)
	}
}

// One exit is absorbed by the supervisor's restart (a lower mount refused
// because the reverse forward was a moment late); the second attempt serves
// and Prepare succeeds.
func TestPrepareAbsorbsOneChildExit(t *testing.T) {
	m := childManager(t, "fail-once")
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, prepareFor(t, coldExport)); err != nil {
		t.Fatalf("Prepare after one exit: %v", err)
	}
	stop(m, "alice", coldExport)
}

// Two exits in a row before the mount answered end Prepare with the last one's
// words, and the failed union is gone.
func TestPrepareReportsTheSecondExit(t *testing.T) {
	m := childManager(t, "fail")
	_, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, prepareFor(t, coldExport))
	if err == nil || !strings.Contains(err.Error(), "fuse-overlayfs is not in the image") {
		t.Fatalf("Prepare = %v, want the child's own words", err)
	}
	if _, ok := m.share("alice", thisClient, coldExport); ok {
		t.Error("the failed union stayed registered")
	}
}

// A child that stays up and mounts is unchanged: Prepare answers once the
// mount does, and the supervisor keeps the child.
func TestPrepareWithAChildThatServes(t *testing.T) {
	m := childManager(t, "serve")
	var up atomic.Bool
	m.probe = func(context.Context, union.Spec) error {
		if up.Load() {
			return nil
		}
		return errors.New("not mounted")
	}
	m.onStart = func(union.Spec) { time.AfterFunc(300*time.Millisecond, func() { up.Store(true) }) }

	merged, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, prepareFor(t, coldExport))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if merged == "" {
		t.Error("no merged path")
	}
	stop(m, "alice", coldExport)
}

// A child that exits after the mount answered is restarted, not reported:
// Prepare has already returned, and the supervisor starts it again.
func TestChildExitAfterReadyIsRestarted(t *testing.T) {
	m := childManager(t, "fail")
	var starts atomic.Int32
	m.onStart = func(union.Spec) { starts.Add(1) }
	m.probe = func(context.Context, union.Spec) error { return nil }

	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, prepareFor(t, coldExport)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// The probe says mounted, so a later probe failure is what a crash looks
	// like; the supervisor must still be running the loop.
	m.mu.Lock()
	l := m.shares[key("alice", thisClient, coldExport)]
	m.mu.Unlock()
	select {
	case <-l.done:
		t.Error("the supervisor ended after the child exited")
	case <-time.After(3 * time.Second):
	}
	stop(m, "alice", coldExport)
}
