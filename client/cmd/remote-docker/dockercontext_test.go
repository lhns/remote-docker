package main

import (
	"errors"
	"slices"
	"testing"
)

// There is always a docker CLI to run, because this binary is one.
//
// Giving up when PATH had none is what left a machine with nothing installed,
// which is the premise of this project, with no docker context at all: every
// tool that resolves one then found the platform default and reported that the
// daemon was not running.
func TestDockerInvocation(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/docker", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	self := func() (string, error) { return "/opt/remote-docker", nil }

	t.Run("a docker on PATH is used as it is", func(t *testing.T) {
		name, argv := dockerInvocation(found, self, []string{"context", "use", "dev"})
		if name != "/usr/bin/docker" {
			t.Errorf("ran %q, want the docker on PATH", name)
		}
		// No "docker" in front. A real CLI has no such subcommand, and the
		// command would fail on an argument we added.
		if !slices.Equal(argv, []string{"context", "use", "dev"}) {
			t.Errorf("argv = %v, want the arguments unchanged", argv)
		}
	})

	t.Run("without one, this binary runs itself", func(t *testing.T) {
		name, argv := dockerInvocation(missing, self, []string{"context", "use", "dev"})
		if name != "/opt/remote-docker" {
			t.Errorf("ran %q, want this binary", name)
		}
		// The same arguments as a real docker gets. This binary's root IS the
		// Docker CLI (ADR 0024), so there is no subcommand to put in front and
		// no shift to get wrong.
		if !slices.Equal(argv, []string{"context", "use", "dev"}) {
			t.Errorf("argv = %v, want the arguments unchanged", argv)
		}
	})

	t.Run("with neither, it still names something", func(t *testing.T) {
		broken := func() (string, error) { return "", errors.New("no") }
		name, argv := dockerInvocation(missing, broken, []string{"context", "ls"})
		if name != "docker" {
			t.Errorf("ran %q, want a plain docker to fail on", name)
		}
		if !slices.Equal(argv, []string{"context", "ls"}) {
			t.Errorf("argv = %v", argv)
		}
	})
}
