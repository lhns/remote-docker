package main

import (
	"path/filepath"
	"testing"
)

// The name decides everything, so the name parsing is worth a table.
//
// The extension and the case are both Windows facts: `DOCKER.EXE` typed at a
// prompt is the same program, and argv[0] may arrive spelled either way
// depending on whether a shell or a launcher started it.
func TestIsDockerName(t *testing.T) {
	for _, tc := range []struct {
		arg0 string
		want bool
	}{
		{"docker", true},
		{"docker.exe", true},
		{"DOCKER.EXE", true},
		{"/usr/local/bin/docker", true},
		{"./docker", true},

		{"remote-docker", false},
		{"remote-docker.exe", false},
		{"/usr/local/bin/remote-docker", false},
		{"docker-compose", false},
		{"dockerd", false},
		{"", false},
	} {
		if got := isDockerName(tc.arg0); got != tc.want {
			t.Errorf("isDockerName(%q) = %v, want %v", tc.arg0, got, tc.want)
		}
	}
}

// A backslash path is only a path where a backslash separates paths.
//
// Asserted on Windows and NOT on Linux, because there `C:\Users\me\docker.exe`
// is one ordinary filename with backslashes in it -- and a file may legally be
// named that. Treating it as a path on both would mean answering to a name
// nobody on Linux could have invoked us by, which is worse than not answering.
func TestWindowsPathsAreOnlyPathsOnWindows(t *testing.T) {
	if filepath.Separator != '\\' {
		t.Skip("backslash is an ordinary filename character here")
	}
	for _, tc := range []struct {
		arg0 string
		want bool
	}{
		{`C:\Users\me\bin\docker.exe`, true},
		{`C:\tools\remote-docker.exe`, false},
	} {
		if got := isDockerName(tc.arg0); got != tc.want {
			t.Errorf("isDockerName(%q) = %v, want %v", tc.arg0, got, tc.want)
		}
	}
}
