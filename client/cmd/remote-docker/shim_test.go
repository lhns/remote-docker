package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinary stands in for remote-docker itself. Nothing here executes it --
// which is the point: identifying what is at the shim's path must never mean
// running it.
func fakeBinary(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// shimIn points the shim at a temporary directory, so no test touches the
// user's real one.
func shimIn(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	t.Setenv("REMOTE_DOCKER_SHIM_DIR", dir)
	return dir
}

func TestInstallCreatesAShimThatLeadsBackToThisBinary(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	dir := shimIn(t)

	var out bytes.Buffer
	if err := installShim(&out, nil, self, false); err != nil {
		t.Fatalf("installShim: %v\n%s", err, out.String())
	}

	got, err := inspectShim(self)
	if err != nil {
		t.Fatalf("inspectShim: %v", err)
	}
	if !got.exists {
		t.Fatal("nothing was installed")
	}
	if !got.ours || !got.current {
		t.Errorf("the shim does not lead back to this binary: %+v", got)
	}
	if got.path != filepath.Join(dir, shimName()) {
		t.Errorf("installed at %s, want it in %s", got.path, dir)
	}
	// A symlink where one can be made, a hardlink where one cannot. Both are
	// correct; a copy without being asked is not.
	if got.form == formCopy {
		t.Errorf("it copied the binary without being asked:\n%s", out.String())
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	shimIn(t)

	var first bytes.Buffer
	if err := installShim(&first, nil, self, false); err != nil {
		t.Fatalf("first install: %v", err)
	}

	var second bytes.Buffer
	if err := installShim(&second, nil, self, false); err != nil {
		t.Fatalf("second install: %v\n%s", err, second.String())
	}
	if !strings.Contains(second.String(), "already installed") {
		t.Errorf("a second install did not report the existing one:\n%s", second.String())
	}
}

// The invariant of the whole feature. A machine may get Docker Desktop
// tomorrow, and a shim that overwrote a real CLI is a broken machine -- so
// anything without our marker beside it is left exactly where it is.
func TestInstallRefusesADockerItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	dir := shimIn(t)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stranger := fakeBinary(t, dir, shimName(), "a real docker CLI")

	var out bytes.Buffer
	err := installShim(&out, nil, self, false)
	if err == nil {
		t.Fatal("it replaced a docker it did not write")
	}
	if !strings.Contains(err.Error(), stranger) {
		t.Errorf("the refusal does not name the file:\n%v", err)
	}

	data, readErr := os.ReadFile(stranger)
	if readErr != nil || string(data) != "a real docker CLI" {
		t.Errorf("the stranger's file was modified: %q, %v", data, readErr)
	}
}

// And uninstall refuses the same file for the same reason: it is the path a
// user reaches for when something is wrong, which is exactly when a
// destructive mistake would be worst.
func TestUninstallRefusesADockerItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	dir := shimIn(t)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stranger := fakeBinary(t, dir, shimName(), "a real docker CLI")

	var out bytes.Buffer
	if err := uninstallShim(&out, self); err == nil {
		t.Fatal("it removed a docker it did not write")
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Errorf("the stranger's file is gone: %v", err)
	}
}

func TestUninstallRemovesWhatItInstalled(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	dir := shimIn(t)

	var out bytes.Buffer
	if err := installShim(&out, nil, self, false); err != nil {
		t.Fatalf("installShim: %v", err)
	}
	if err := uninstallShim(&out, self); err != nil {
		t.Fatalf("uninstallShim: %v", err)
	}

	if _, err := os.Lstat(filepath.Join(dir, shimName())); !os.IsNotExist(err) {
		t.Errorf("the shim is still there: %v", err)
	}
	// The bookkeeping goes with it, or the next install would believe a file
	// somebody else put there was ours.
	if _, err := os.Stat(markerPath(dir)); !os.IsNotExist(err) {
		t.Errorf("the marker is still there: %v", err)
	}
}

// A copy is the one form that duplicates the binary and silently goes stale,
// so it is never reached without consent -- and "no terminal to ask" must mean
// no, which is what CI and every script are.
func TestACopyIsNeverMadeWithoutConsent(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	dir := shimIn(t)

	if err := installCopy(&bytes.Buffer{}, self, dir); err != nil {
		t.Fatalf("preparing the copy case: %v", err)
	}

	var out bytes.Buffer
	if confirm(&out, nil, "Copy the binary anyway?") {
		t.Error("a missing input was read as yes")
	}
	if confirm(&out, strings.NewReader(""), "Copy the binary anyway?") {
		t.Error("a closed input was read as yes")
	}
	if confirm(&out, strings.NewReader("\n"), "Copy the binary anyway?") {
		t.Error("a bare newline was read as yes")
	}
	if confirm(&out, strings.NewReader("no\n"), "Copy the binary anyway?") {
		t.Error("\"no\" was read as yes")
	}
	if !confirm(&out, strings.NewReader("y\n"), "Copy the binary anyway?") {
		t.Error("\"y\" was not read as yes")
	}
}

// A hardlink or a copy keeps serving the build that existed when it was made,
// and an upgrade says nothing about it. `status` is the only thing that can,
// so it has to notice.
func TestAShimPointingAtAnOlderBuildIsReportedStale(t *testing.T) {
	home := t.TempDir()
	self := fakeBinary(t, home, "remote-docker", "binary")
	dir := shimIn(t)

	// A copy of an older build, with our marker beside it: this is exactly
	// what an upgrade leaves behind.
	if err := installCopy(&bytes.Buffer{}, self, dir); err != nil {
		t.Fatal(err)
	}

	upgraded := fakeBinary(t, home, "remote-docker-new", "a newer binary")
	got, err := inspectShim(upgraded)
	if err != nil {
		t.Fatalf("inspectShim: %v", err)
	}
	if !got.ours {
		t.Fatal("our own copy was not recognised as ours")
	}
	if got.current {
		t.Error("a copy of an older build was reported as current")
	}

	var out bytes.Buffer
	reportShim(&out, upgraded)
	if !strings.Contains(out.String(), "STALE") {
		t.Errorf("status did not report the stale shim:\n%s", out.String())
	}
}

// installCopy is the copy rung on its own, which is the only way to reach it
// in a test without a second volume.
func installCopy(out *bytes.Buffer, self, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, shimName())
	if err := copyFile(self, path); err != nil {
		return err
	}
	_, _ = out.WriteString("copied\n")
	return writeMarker(dir, marker{Form: string(formCopy), From: self, Version: version})
}
