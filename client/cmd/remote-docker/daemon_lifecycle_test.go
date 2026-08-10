package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sleeper starts a long-lived process that is NOBODY'S child, and returns its
// pid.
//
// Both halves of that matter, and the second one cost a red CI run. A session
// is orphaned by the time anything asks about it: `start` releases the child
// and exits, so init reaps it. A test that keeps the process as its own child
// tests something else -- on Unix a killed child stays in the process table as
// a zombie until its parent waits, and processAlive answers about the process
// table, so `killPID` worked and the test watched the corpse for ten seconds
// and failed.
//
// So this spawns a grandchild: the middle process starts the sleeper, prints
// its pid and exits, which orphans it exactly as `start` does. The sleeper is
// this test binary re-executed, which is the only way to get a long-lived
// process on every platform this client builds for.
func sleeper(t *testing.T) int {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSpawnsASleeper")
	cmd.Env = append(os.Environ(), "RD_HELPER_SPAWN=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("spawning the helper: %v", err)
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		rest, found := strings.CutPrefix(scanner.Text(), "SLEEPER_PID ")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(rest)
		if err != nil {
			t.Fatalf("unreadable pid %q: %v", rest, err)
		}
		return pid
	}
	t.Fatalf("the helper did not report a pid; it said:\n%s", out)
	return 0
}

// A start that times out must not leave the child running.
//
// Returning the error on its own leaves a session that is merely SLOW rather
// than dead: it binds the endpoint a moment after the user was told it failed,
// and the next `start` then reports "already running" and points at the very
// session its predecessor disowned. Nothing on screen ever named that process.
//
// The process here is orphaned exactly as startDaemon's is, so the test
// exercises the case that made killing by pid necessary: this process is not
// its parent and cannot wait on it.
func TestKillPIDEndsAChildWeStoppedParenting(t *testing.T) {
	pid := sleeper(t)

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

	pid := sleeper(t)

	if waitForExit(pid, 300*time.Millisecond) {
		t.Error("waitForExit reported a running process as gone")
	}

	if err := killPID(pid); err != nil {
		t.Fatalf("killPID: %v", err)
	}

	if !waitForExit(pid, 10*time.Second) {
		t.Error("waitForExit did not notice the process had gone")
	}
}

// TestHelperSpawnsASleeper is not a test. It starts the sleeper, reports its
// pid and exits, which is what leaves the sleeper orphaned.
func TestHelperSpawnsASleeper(t *testing.T) {
	if os.Getenv("RD_HELPER_SPAWN") == "" {
		t.Skip("helper process; runs only when re-executed")
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperSleeps")
	cmd.Env = append(os.Environ(), "RD_HELPER_SLEEP=1")
	// Stdout is left nil deliberately: inheriting this process's pipe would
	// hold it open for the sleeper's whole life, and the reader waiting on
	// EOF would wait exactly that long.
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the sleeper: %v", err)
	}
	fmt.Println("SLEEPER_PID", cmd.Process.Pid)
	_ = cmd.Process.Release()
}

// TestHelperSleeps is not a test either. It is the long-lived process the ones
// above need, and it exits immediately unless re-executed on purpose. The
// minute is a backstop: every test that starts one also kills it.
func TestHelperSleeps(t *testing.T) {
	if os.Getenv("RD_HELPER_SLEEP") == "" {
		t.Skip("helper process; runs only when re-executed")
	}
	time.Sleep(60 * time.Second)
}
