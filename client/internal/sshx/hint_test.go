package sshx

// The one thing an authentication failure has to say.
//
// The workspace enrols a key by filename, out of band, so a refusal is nearly
// always a key that is not in the directory yet or a file that has just been
// written and not yet re-read. x/crypto's message names none of that.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-client/keys"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	key, err := keys.LoadOrCreateKey(filepath.Join(t.TempDir(), "id_ed25519"), "test")
	if err != nil {
		t.Fatal(err)
	}
	return Config{Host: "workspace.example", Port: 2222, User: "alice", Key: key}
}

func TestAnAuthFailureSaysHowToEnrol(t *testing.T) {
	cfg := testConfig(t)
	hint := enrolmentHint(errors.New(
		"ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain"),
		cfg)

	// The file, because the name of it is the whole instruction.
	if !strings.Contains(hint, "authorized_keys.d/alice.pub") {
		t.Errorf("the hint does not name the file to create:\n%s", hint)
	}
	// The key, because a machine may have more than one and only one of them
	// was offered.
	if !strings.Contains(hint, ssh.FingerprintSHA256(cfg.Key.Signer.PublicKey())) {
		t.Errorf("the hint does not say which key was offered:\n%s", hint)
	}
	// The wait, which is the part that made somebody give up and change
	// something that was already correct.
	if !strings.Contains(hint, "minute") {
		t.Errorf("the hint does not mention that enrolment is not instant:\n%s", hint)
	}
}

// Every other failure is left alone. A refused host key or an unreachable port
// has nothing to do with enrolment, and pointing at it would send the reader
// the wrong way.
func TestOtherFailuresGetNoHint(t *testing.T) {
	cfg := testConfig(t)
	for _, err := range []error{
		nil,
		errors.New("ssh: handshake failed: knownhosts: key mismatch"),
		errors.New("dial tcp 10.0.0.1:2222: connect: connection refused"),
	} {
		if hint := enrolmentHint(err, cfg); hint != "" {
			t.Errorf("%v was given an enrolment hint:\n%s", err, hint)
		}
	}
}

// A config with no key at all must not panic on the way to reporting a
// failure.
func TestNoKeyMeansNoHint(t *testing.T) {
	if hint := enrolmentHint(errors.New("ssh: unable to authenticate"), Config{User: "alice"}); hint != "" {
		t.Errorf("a keyless config produced a hint:\n%s", hint)
	}
}
