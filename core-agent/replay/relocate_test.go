package replay

import (
	"errors"
	"strings"
	"testing"
)

// A per-account daemon reports a mountpoint in ITS filesystem, which the agent
// cannot open by that path. The root is how it reaches it, and this is the
// whole of what changes for the replayer (ADR 0019).
func TestLocateMapsAMountpointIntoTheDaemonsRoot(t *testing.T) {
	got, err := Locate("/var/lib/docker/volumes/rd-1111111111111111/_data",
		func() (string, error) { return "/proc/42/root", nil })
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	want := Mount{Trusted: "/proc/42/root", Path: "/var/lib/docker/volumes/rd-1111111111111111/_data"}
	if got != want {
		t.Errorf("locate = %+v, want %+v", got, want)
	}
}

// With no root (the shared daemon) the path is already ours.
//
// All three spellings of "no relocation" must work, and "/" is here because it
// did not. daemons.Shared reports its root as "/", which is true; this function
// took it for a prefix and asked whether the mountpoint began with "//", which
// nothing does. Every replay on a shared-daemon workspace was refused as an
// escape attempt, and the integration suite is what said so. The unit tests
// asserted the value "/" without ever passing it through the code that reads
// it, which is a test agreeing with a bug rather than catching it.
func TestLocateLeavesOurOwnPathsAlone(t *testing.T) {
	const mp = "/var/lib/docker/volumes/rd-1111111111111111/_data"

	for _, root := range []func() (string, error){
		nil,
		func() (string, error) { return "", nil },
		func() (string, error) { return "/", nil },
	} {
		if got, err := Locate(mp, root); err != nil || got != (Mount{Trusted: mp}) {
			t.Errorf("locate = %+v, %v; want the mountpoint trusted whole", got, err)
		}
	}
}

// A daemon we cannot locate must fail rather than fall back to the
// unrelocated path. That path EXISTS in the agent's filesystem: it is the
// shared daemon's, so a silent fallback would replay one account's edits
// into another daemon's volume.
func TestLocateFailsRatherThanFallingBack(t *testing.T) {
	boom := errors.New("no such daemon")

	got, err := Locate("/var/lib/docker/volumes/rd-1111111111111111/_data",
		func() (string, error) { return "", boom })
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to carry the cause", err)
	}
	if got != (Mount{}) {
		t.Errorf("a mount was returned alongside the error: %+v", got)
	}
}

// A mountpoint that climbs is cleaned against the daemon's root, so it can
// never name anything outside it.
//
// The mountpoint is whatever the account's daemon says, and in per-account mode
// the account is root inside that daemon's container, so this is
// attacker-controlled input to a root process choosing a path to touch. Joining
// it to the root would look like containment and is not (path.Join cleans, and
// "/proc/42/root" + "/../../etc/shadow" is "/proc/etc/shadow"), which is why
// the two stay apart.
func TestLocateKeepsAClimbingMountpointInsideTheRoot(t *testing.T) {
	for _, mp := range []string{"/../../etc/shadow", "/..", "/../../../", "/a/../../.."} {
		got, err := Locate(mp, func() (string, error) { return "/proc/42/root", nil })
		if err != nil {
			t.Fatalf("locate(%q): %v", mp, err)
		}
		if got.Trusted != "/proc/42/root" || strings.Contains(got.Path, "..") || !strings.HasPrefix(got.Path, "/") {
			t.Errorf("locate(%q) = %+v; the path must stay rooted and clean", mp, got)
		}
	}
}
