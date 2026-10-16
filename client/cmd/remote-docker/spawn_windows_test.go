//go:build windows

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	rstrtmgr                = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = rstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = rstrtmgr.NewProc("RmRegisterResources")
	procRmShutdown          = rstrtmgr.NewProc("RmShutdown")
	procRmEndSession        = rstrtmgr.NewProc("RmEndSession")
)

// rmShutdown asks Restart Manager to stop whatever runs file, as Windows
// Installer does for a file it is about to replace, without forcing it.
func rmShutdown(t *testing.T, file string) uintptr {
	t.Helper()
	var session uint32
	key := make([]uint16, 33) // CCH_RM_SESSION_KEY + 1
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 {
		t.Fatalf("RmStartSession: %d", r)
	}
	defer func() { _, _, _ = procRmEndSession.Call(uintptr(session)) }()
	name, err := windows.UTF16PtrFromString(file)
	if err != nil {
		t.Fatal(err)
	}
	files := []*uint16{name}
	if r, _, _ := procRmRegisterResources.Call(uintptr(session), 1, uintptr(unsafe.Pointer(&files[0])), 0, 0, 0, 0); r != 0 {
		t.Fatalf("RmRegisterResources: %d", r)
	}
	r, _, _ := procRmShutdown.Call(uintptr(session), 0, 0)
	return r
}

// An MSI upgrade replacing remote-docker.exe stops a background session the
// way Restart Manager stops any console program, with CTRL_C_EVENT, and the
// session takes it as Ctrl-C. Started DETACHED_PROCESS it had no console to
// send that to, and the upgrade reported it could not close it (ADR 0048).
func TestRestartManagerStopsABackgroundSession(t *testing.T) {
	if os.Getenv("RD_RM_CHILD") != "" {
		t.Skip("the child of this test")
	}
	for _, tc := range []struct {
		name   string
		attr   func(*exec.Cmd)
		stops  bool
		reason string
	}{
		{"as a background session is started", detach, true, ""},
		{"with no console", func(c *exec.Cmd) {
			c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
		}, false, "the flags detach used before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A copy, so the only process running the file is the child and
			// Restart Manager does not stop this test as well.
			child := filepath.Join(dir, "child.exe")
			copyFile(t, os.Args[0], child)

			cmd := exec.Command(child, "-test.run=^TestRestartManagerChild$")
			cmd.Env = append(os.Environ(), "RD_RM_CHILD="+dir)
			tc.attr(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			defer func() { _ = cmd.Process.Kill() }()

			waitForFile(t, filepath.Join(dir, "ready"))
			r := rmShutdown(t, child)

			if !tc.stops {
				if r == 0 {
					t.Fatalf("Restart Manager stopped a process %s; the test no longer shows why detach changed", tc.reason)
				}
				t.Logf("RmShutdown = %d", r)
				return
			}
			if r != 0 {
				t.Fatalf("RmShutdown = %d (351 is ERROR_FAIL_SHUTDOWN)", r)
			}
			select {
			case err := <-exited:
				if err != nil {
					t.Fatalf("the child did not exit cleanly: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the child is still running")
			}
			if _, err := os.Stat(filepath.Join(dir, "interrupted")); err != nil {
				t.Fatal("the child exited without seeing an interrupt")
			}
		})
	}
}

// TestRestartManagerChild is the session's stand-in: the same signal setup,
// and a record of having been interrupted.
func TestRestartManagerChild(t *testing.T) {
	dir := os.Getenv("RD_RM_CHILD")
	if dir == "" {
		t.Skip("run by TestRestartManagerStopsABackgroundSession")
	}
	acceptCtrlC()
	ctx, cancel := signalContext()
	defer cancel()
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if err := os.WriteFile(filepath.Join(dir, "interrupted"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("never interrupted")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}
