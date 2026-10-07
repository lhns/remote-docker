package dircache

import (
	"os/exec"
	"testing"
)

// A junction needs no privilege, so this is the link a Windows test can always
// make.
func init() {
	linkMakers = append(linkMakers, linkMaker{"junction", func(t *testing.T, link, target string) {
		t.Helper()
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v: %s", err, out)
		}
	}})
}
