package notify

import (
	"errors"
	"strings"
	"testing"
)

// A per-account daemon reports a mountpoint in ITS filesystem, which the agent
// cannot open by that path. The root is how it reaches it, and this is the
// whole of what changes for the replayer (ADR 0019).
func TestRelocateMapsAMountpointIntoTheDaemonsRoot(t *testing.T) {
	got, err := relocate("/var/lib/docker/volumes/rd-cwd/_data",
		func() (string, error) { return "/proc/42/root", nil })
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if want := "/proc/42/root/var/lib/docker/volumes/rd-cwd/_data"; got != want {
		t.Errorf("relocate = %q, want %q", got, want)
	}
}

// With no root -- the shared daemon -- the path is already ours.
func TestRelocateLeavesOurOwnPathsAlone(t *testing.T) {
	const mp = "/var/lib/docker/volumes/rd-cwd/_data"

	if got, err := relocate(mp, nil); err != nil || got != mp {
		t.Errorf("relocate(nil root) = %q, %v; want it unchanged", got, err)
	}
	if got, err := relocate(mp, func() (string, error) { return "", nil }); err != nil || got != mp {
		t.Errorf("relocate(empty root) = %q, %v; want it unchanged", got, err)
	}
}

// A daemon we cannot locate must fail rather than fall back to the
// unrelocated path. That path EXISTS in the agent's filesystem -- it is the
// shared daemon's -- so a silent fallback would replay one account's edits
// into another daemon's volume.
func TestRelocateFailsRatherThanFallingBack(t *testing.T) {
	boom := errors.New("no such daemon")

	got, err := relocate("/var/lib/docker/volumes/rd-cwd/_data",
		func() (string, error) { return "", boom })
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to carry the cause", err)
	}
	if got != "" {
		t.Errorf("a path was returned alongside the error: %q", got)
	}
}

// A mountpoint that climbs out of the root must be refused, not cleaned.
//
// This is the test that found the hole. Joining looks like containment and is
// not: path.Join CLEANS, so "/proc/42/root" joined to "/../../etc/shadow" is
// "/proc/etc/shadow" -- outside the root, no error, looking right.
//
// It matters because of what this input is. The mountpoint is whatever the
// account's daemon says, and in per-account mode the account is root inside
// that daemon's container, so this is attacker-controlled input to a root
// process choosing a path to touch.
func TestRelocateRefusesAPathThatLeavesTheRoot(t *testing.T) {
	// Only paths that actually leave. "/var/lib/docker/../../../etc" is NOT
	// one of them: its .. are absorbed before they reach the root, landing on
	// /etc inside the account's own container, which is the account's own
	// business.
	for _, mp := range []string{
		"/../../etc/shadow",
		"/..",
		"/../../../",
	} {
		got, err := relocate(mp, func() (string, error) { return "/proc/42/root", nil })
		if err == nil {
			t.Errorf("relocate(%q) = %q with no error; it escaped the daemon's root", mp, got)
		}
		if got != "" {
			t.Errorf("relocate(%q) returned %q alongside its error", mp, got)
		}
	}
}

// And a path that merely SHARES A PREFIX with the root is not inside it.
// "/proc/42/rootkit" starts with "/proc/42/root" and is a different directory.
func TestRelocateDoesNotAcceptASiblingOfTheRoot(t *testing.T) {
	got, err := relocate("kit/x", func() (string, error) { return "/proc/42/root", nil })
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	if !strings.HasPrefix(got, "/proc/42/root/") {
		t.Errorf("relocate = %q, which is not under the root", got)
	}
}
