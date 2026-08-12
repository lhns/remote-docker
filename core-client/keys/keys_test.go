package keys

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestLoadOrCreateKeyGeneratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")

	first, err := LoadOrCreateKey(path, "remote-docker-test")
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if first.Signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %q, want %q", first.Signer.PublicKey().Type(), ssh.KeyAlgoED25519)
	}

	// The public half is enrolled on the workspace, so regenerating would
	// silently revoke this machine's access.
	second, err := LoadOrCreateKey(path, "remote-docker-test")
	if err != nil {
		t.Fatalf("second LoadOrCreateKey: %v", err)
	}
	if string(first.Signer.PublicKey().Marshal()) != string(second.Signer.PublicKey().Marshal()) {
		t.Error("a second call generated a different key, revoking the enrolled one")
	}
}

func TestLoadOrCreateKeyWritesPublicHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")

	kp, err := LoadOrCreateKey(path, "remote-docker-host-user")
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}

	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatalf("reading public key: %v", err)
	}
	// This file is what the user hands over for enrolment, so it must be a
	// valid authorized_keys line and carry the comment identifying the
	// machine it came from.
	parsed, comment, _, _, err := ssh.ParseAuthorizedKey(pub)
	if err != nil {
		t.Fatalf("public key is not a valid authorized_keys line: %v", err)
	}
	if comment != "remote-docker-host-user" {
		t.Errorf("comment = %q, want %q", comment, "remote-docker-host-user")
	}
	if string(parsed.Marshal()) != string(kp.Signer.PublicKey().Marshal()) {
		t.Error("the written public key does not match the signer")
	}
}

func TestAuthorizedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	kp, err := LoadOrCreateKey(path, "")
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}

	line := kp.AuthorizedKey("alice@laptop")
	if strings.Contains(line, "\n") {
		t.Errorf("AuthorizedKey() = %q, want a single line", line)
	}
	if _, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err != nil {
		t.Errorf("AuthorizedKey() is not parseable: %v", err)
	} else if comment != "alice@laptop" {
		t.Errorf("comment = %q, want %q", comment, "alice@laptop")
	}

	if got := kp.AuthorizedKey(""); strings.HasSuffix(got, " ") {
		t.Errorf("AuthorizedKey(\"\") = %q, want no trailing separator", got)
	}
}

func TestLoadOrCreateKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Silently regenerating over an unreadable key would revoke access; the
	// user has to be told instead.
	if _, err := LoadOrCreateKey(path, ""); err == nil {
		t.Error("LoadOrCreateKey over a corrupt key = nil error, want an error")
	}
}

func TestPrivateKeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on Windows")
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if _, err := LoadOrCreateKey(path, ""); err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key mode = %04o, want 0600", perm)
	}
}
