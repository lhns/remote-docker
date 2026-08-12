package accounts

// What a key file is allowed to look like halfway through being written, and
// what it takes to revoke on one.
//
// The reported failure: on Windows the file is created first and filled a
// moment later, and the sync that lands in between used to revoke the account
// and log that the file was gone.

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// keyLine generates a key and its authorized_keys line.
func keyLine(t *testing.T) (ssh.PublicKey, []byte) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key, ssh.MarshalAuthorizedKey(key)
}

// write replaces a key file's contents.
func (s *testStore) write(t *testing.T, filename string, content []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.keysDir, filename), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// authorized reports whether the account still accepts the key.
func (s *testStore) authorized(t *testing.T, name string, key ssh.PublicKey) bool {
	t.Helper()
	a, ok := s.Lookup(name)
	if !ok {
		t.Fatalf("%s is not an account at all", name)
	}
	return a.Authorized(key)
}

// The reported bug. An editor truncates before it writes, so a sync can land on
// an empty file, and one read cannot tell that from an emptying meant on
// purpose. It takes two.
func TestAnEmptyFileRevokesOnTheSecondReadNotTheFirst(t *testing.T) {
	s := newStore(t)
	key := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	// The save, caught halfway.
	s.write(t, "alice.pub", nil)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if !s.authorized(t, "alice", key) {
		t.Fatal("a file caught mid-write revoked the account")
	}

	// Filled in, and nothing ever happened.
	_, line := keyLine(t)
	s.write(t, "alice.pub", line)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if s.authorized(t, "alice", key) {
		t.Error("the old key is still accepted after the file was replaced")
	}

	// And now emptied on purpose: seen empty twice, so revoked.
	s.write(t, "alice.pub", nil)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Lookup("alice"); len(a.Keys) != 0 {
		t.Error("an emptied key file did not revoke the account")
	}
}

// Garbage is the same case: it may be a paste that has not finished.
func TestGarbageAlsoTakesTwoReads(t *testing.T) {
	s := newStore(t)
	key := s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}

	s.write(t, "alice.pub", []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI"))
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if !s.authorized(t, "alice", key) {
		t.Error("a half-pasted key revoked the account on the first read")
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Lookup("alice"); len(a.Keys) != 0 {
		t.Error("a key file that stayed unreadable did not revoke")
	}
}

// A file that is GONE revokes at once. There is no write window to be caught
// in, and waiting would mean an intended revocation takes a poll interval
// longer than it should.
func TestARemovedFileRevokesImmediately(t *testing.T) {
	s := newStore(t)
	s.writeKey(t, "alice.pub")
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.keysDir, "alice.pub")); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Lookup("alice"); len(a.Keys) != 0 {
		t.Error("a removed key file did not revoke on the first read")
	}
}

// The account name belongs to the file that holds it, including while that file
// is empty. Otherwise a colliding file takes the account during somebody's
// save, which is the takeover the collision rule exists to refuse.
//
// Two names that differ on every filesystem, for the reason
// TestSyncRefusesCollidingNames gives: alice.pub and Alice.pub are one file on
// Windows, and this would measure nothing there.
func TestAMidWriteFileKeepsItsName(t *testing.T) {
	s := newStore(t)
	s.write(t, "alice-smith.pub", nil)
	other := s.writeKey(t, "alice.smith.pub")

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if a, ok := s.Lookup("alice-smith"); ok && a.Authorized(other) {
		t.Error("a colliding file took the account while the real one was being written")
	}
}

// Several keys per file is the format, and one bad line costs that line only.
// Reading the file as a single stream instead stops at the first line it cannot
// parse, silently dropping every key below it: a typo or a BOM on line one then
// revokes an account nobody touched.
func TestOneBadLineCostsOneKey(t *testing.T) {
	s := newStore(t)
	first, a := keyLine(t)
	second, b := keyLine(t)

	var file []byte
	file = append(file, []byte("# alice's laptop\n")...)
	file = append(file, a...)
	file = append(file, []byte("ssh-ed25519 this-is-not-a-key\n\n")...)
	file = append(file, b...)
	s.write(t, "alice.pub", file)

	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if !s.authorized(t, "alice", first) {
		t.Error("the key above the bad line was lost")
	}
	if !s.authorized(t, "alice", second) {
		t.Error("the key below the bad line was lost")
	}
}

// CRLF, because the file is written on Windows.
func TestWindowsLineEndingsParse(t *testing.T) {
	key, line := keyLine(t)
	path := filepath.Join(t.TempDir(), "alice.pub")
	crlf := strings.ReplaceAll(string(line), "\n", "\r\n")
	if err := os.WriteFile(path, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}

	keys, skipped, err := parseKeys(path)
	if err != nil {
		t.Fatalf("a CRLF key file was refused: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped %d lines, want 0", skipped)
	}
	if len(keys) != 1 || ssh.FingerprintSHA256(keys[0]) != ssh.FingerprintSHA256(key) {
		t.Error("the key did not survive CRLF")
	}
}

// The byte order mark, which is invisible in every editor that writes one.
func TestByteOrderMarks(t *testing.T) {
	key, line := keyLine(t)

	t.Run("utf-8 is stripped", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alice.pub")
		if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, line...), 0o644); err != nil {
			t.Fatal(err)
		}
		keys, _, err := parseKeys(path)
		if err != nil {
			t.Fatalf("a UTF-8 BOM refused the file: %v", err)
		}
		if len(keys) != 1 || ssh.FingerprintSHA256(keys[0]) != ssh.FingerprintSHA256(key) {
			t.Error("the key did not survive the BOM")
		}
	})

	t.Run("utf-16 is named", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "alice.pub")
		var utf16 []byte
		utf16 = append(utf16, 0xff, 0xfe)
		for _, b := range line {
			utf16 = append(utf16, b, 0x00)
		}
		if err := os.WriteFile(path, utf16, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := parseKeys(path)
		if err == nil {
			t.Fatal("a UTF-16 key file was accepted")
		}
		// The encoding by name: nothing about looking at the file shows it.
		if !strings.Contains(err.Error(), "UTF-16") {
			t.Errorf("the error does not name the encoding: %v", err)
		}
	})
}

// An empty file and a file of junk are different problems and must read as
// different problems.
func TestTheReasonNamesWhatIsWrong(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.pub")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseKeys(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty file reads as: %v", err)
	}

	junk := filepath.Join(dir, "junk.pub")
	if err := os.WriteFile(junk, []byte("hello\nthere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, skipped, err := parseKeys(junk)
	if err == nil {
		t.Fatal("a file of prose was accepted")
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
}
