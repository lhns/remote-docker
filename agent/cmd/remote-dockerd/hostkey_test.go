package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// `token create` and a first `serve` can both find no host key. The one that
// writes second must not replace the key the first is already serving, or
// the invite pins a key nobody serves.
func TestGenerateHostKeyKeepsAKeyWrittenMeanwhile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh_host_ed25519_key")
	first, err := generateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateHostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("the second generation replaced the host key the first wrote")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, first) {
		t.Fatal("the key on disk is not the one the first generation returned")
	}
	if left, _ := filepath.Glob(path + ".*"); len(left) > 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}
