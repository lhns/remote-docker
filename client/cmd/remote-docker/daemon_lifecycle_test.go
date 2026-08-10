package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// A start that times out must not leave the child running.
//
// Returning the error on its own leaves a session that is merely SLOW rather
// than dead: it binds the endpoint a moment after the user was told it failed,
// and the next `start` then reports "already running" and points at the very
// session its predecessor disowned. Nothing on screen ever named that process.
//
// The child here is this test binary re-executed to sleep, which is the only
// way to get a long-lived process on every platform this client builds for
// without shelling out to something that exists on some of them.
func TestKillPIDEndsAChildWeStoppedParenting(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleeps")
	cmd.Env = append(os.Environ(), "RD_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}
	pid := cmd.Process.Pid

	// Released exactly as startDaemon does, so the test exercises the case
	// that made killing by pid necessary: this process is no longer the
	// child's parent and cannot wait on it.
	if err := cmd.Process.Release(); err != nil {
		t.Fatalf("releasing the helper: %v", err)
	}

	if !processAlive(pid) {
		t.Fatal("the helper was not running, so this proves nothing")
	}
	if err := killPID(pid); err != nil {
		t.Fatalf("killPID: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatal("the child was still running after killPID")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForExit returns as soon as the process is gone, and says so when it is
// not. The zero pid is "nobody told us", which must not turn `stop` into a
// failure -- an older session does not report one.
func TestWaitForExit(t *testing.T) {
	if !waitForExit(0, time.Second) {
		t.Error("a pid of 0 must be treated as already gone")
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleeps")
	cmd.Env = append(os.Environ(), "RD_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the helper: %v", err)
	}
	pid := cmd.Process.Pid

	if waitForExit(pid, 300*time.Millisecond) {
		t.Error("waitForExit reported a running process as gone")
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	if !waitForExit(pid, 10*time.Second) {
		t.Error("waitForExit did not notice the process had gone")
	}
}

// TestHelperSleeps is not a test. It is the long-lived child the two above
// need, and it exits immediately unless re-executed on purpose.
func TestHelperSleeps(t *testing.T) {
	if os.Getenv("RD_HELPER_SLEEP") == "" {
		t.Skip("helper process; runs only when re-executed")
	}
	time.Sleep(60 * time.Second)
}
