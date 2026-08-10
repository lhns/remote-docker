//go:build !windows

package config

import "testing"

// A workspace's socket must sit beside the default one, keep the .sock
// extension, and stay in the same directory.
//
// Beside rather than below, because one mkdir then covers every workspace.
// Extension preserved because a unix socket path is a filename and tools that
// glob for *.sock are ordinary -- docker.sock-dev would have worked and looked
// wrong, which is the kind of thing nobody reports.
func TestJoinEndpointKeepsTheDirectoryAndTheExtension(t *testing.T) {
	const base = "/run/user/1000/remote-docker/docker.sock"

	if got, want := joinEndpoint(base, "dev"), "/run/user/1000/remote-docker/docker-dev.sock"; got != want {
		t.Errorf("joinEndpoint = %q, want %q", got, want)
	}
	if got := joinEndpoint("/run/rd/docker", "dev"); got != "/run/rd/docker-dev" {
		t.Errorf("a base with no extension = %q, want /run/rd/docker-dev", got)
	}
}
