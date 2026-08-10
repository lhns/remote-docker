package main

import "testing"

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
		{`C:\Users\me\bin\docker.exe`, true},
		{"/usr/local/bin/docker", true},
		{"./docker", true},

		{"remote-docker", false},
		{"remote-docker.exe", false},
		{`C:\tools\remote-docker.exe`, false},
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
