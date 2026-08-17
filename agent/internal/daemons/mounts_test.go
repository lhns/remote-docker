package daemons

import (
	"strings"
	"testing"
)

// The case this exists for: a workspace with an insecure or private registry
// mounts the daemon's configuration in, and every account's daemon needs the
// same files or a pull that works on the workspace fails inside every account.
func TestParseMountsReadsTheDocumentedForm(t *testing.T) {
	got, err := ParseMounts("/etc/docker/daemon.json:/etc/docker/daemon.json:ro, /etc/docker/certs.d:/etc/docker/certs.d")
	if err != nil {
		t.Fatalf("ParseMounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d mounts, want 2: %+v", len(got), got)
	}
	if got[0].Type != "bind" || got[0].Source != "/etc/docker/daemon.json" || !got[0].ReadOnly {
		t.Errorf("first mount is %+v", got[0])
	}
	if got[1].ReadOnly {
		t.Errorf("second mount is read-only without being asked: %+v", got[1])
	}
}

func TestParseMountsIsEmptyWhenUnset(t *testing.T) {
	got, err := ParseMounts("")
	if err != nil || got != nil {
		t.Errorf("ParseMounts(\"\") = %+v, %v", got, err)
	}
}

// A relative source is not a path to docker, it is a volume NAME, so this would
// mount an empty volume and the daemon would read no configuration at all while
// appearing to start correctly.
func TestParseMountsRefusesARelativeSource(t *testing.T) {
	if _, err := ParseMounts("etc/docker:/etc/docker"); err == nil {
		t.Error("a relative source was accepted, which docker reads as a volume name")
	}
}

// Two mounts at one destination is refused by docker, so the daemon would not
// start and the message would name a path rather than the setting behind it.
func TestParseMountsRefusesTheDaemonsOwnPaths(t *testing.T) {
	for _, spec := range []string{
		"/somewhere:" + SocketMount,
		"/somewhere:/var/lib/docker",
	} {
		_, err := ParseMounts(spec)
		if err == nil {
			t.Errorf("%q was accepted over a path the daemon needs for itself", spec)
			continue
		}
		if !strings.Contains(err.Error(), "every daemon needs") {
			t.Errorf("%q was refused for the wrong reason: %v", spec, err)
		}
	}
}

func TestParseMountsRefusesWhatItCannotRead(t *testing.T) {
	for _, spec := range []string{
		"/only-one-path",
		"/a:/b:ro:extra",
		"/a:/b:readonly",
	} {
		if _, err := ParseMounts(spec); err == nil {
			t.Errorf("%q was accepted", spec)
		}
	}
}

// The mounts reach the plan, after the two every daemon has, and change the
// fingerprint: a daemon created before the setting existed is out of date and
// is rebuilt when it is next idle, which is what applies the change at all.
func TestExtraMountsReachThePlanAndTheFingerprint(t *testing.T) {
	mounts, err := ParseMounts("/etc/docker/daemon.json:/etc/docker/daemon.json:ro")
	if err != nil {
		t.Fatal(err)
	}

	plain := plan(t, "alice", Options{})
	with := plan(t, "alice", Options{Mounts: mounts})

	if len(with.Mounts) != len(plain.Mounts)+1 {
		t.Fatalf("got %d mounts, want one more than %d", len(with.Mounts), len(plain.Mounts))
	}
	if with.Mounts[0] != plain.Mounts[0] || with.Mounts[1] != plain.Mounts[1] {
		t.Error("an extra mount displaced the socket directory or the graph volume")
	}
	if args := strings.Join(with.Args(), " "); !strings.Contains(args, "/etc/docker/daemon.json:/etc/docker/daemon.json:ro") {
		t.Errorf("the mount never reached the command line: %s", args)
	}
	if Fingerprint(with) == Fingerprint(plain) {
		t.Error("adding a mount left the fingerprint alone, so no existing daemon would be rebuilt")
	}
}
