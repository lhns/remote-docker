package main

import (
	"errors"
	"testing"
)

// There is always a docker CLI to run, because this binary is one.
//
// Giving up when PATH had none is what left a machine with nothing installed,
// which is the premise of this project, with no docker context at all: every
// tool that resolves one then found the platform default and reported that the
// daemon was not running.
func TestDockerProgram(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/docker", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	self := func() (string, error) { return "/opt/remote-docker", nil }

	t.Run("a docker on PATH is used as it is", func(t *testing.T) {
		if name := dockerProgram(found, self); name != "/usr/bin/docker" {
			t.Errorf("ran %q, want the docker on PATH", name)
		}
	})

	// This binary's root IS the Docker CLI (ADR 0024), so it takes the same
	// arguments a real docker gets and nothing has to be put in front.
	t.Run("without one, this binary runs itself", func(t *testing.T) {
		if name := dockerProgram(missing, self); name != "/opt/remote-docker" {
			t.Errorf("ran %q, want this binary", name)
		}
	})

	t.Run("with neither, it still names something", func(t *testing.T) {
		broken := func() (string, error) { return "", errors.New("no") }
		if name := dockerProgram(missing, broken); name != "docker" {
			t.Errorf("ran %q, want a plain docker to fail on", name)
		}
	})
}
