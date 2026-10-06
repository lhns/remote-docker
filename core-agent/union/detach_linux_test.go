//go:build linux

package union

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// Release must not ask whether a target is mounted before detaching it: that
// question is a stat, and on a lower whose NFS server has gone it waits out the
// soft mount's retries. So detach tries every target lazily and reads EINVAL
// as "not a mount".
func TestDetachTriesEveryTargetLazily(t *testing.T) {
	var tried []string
	unmount := func(target string, flags int) error {
		if flags != unix.MNT_DETACH {
			t.Errorf("%s unmounted with flags %#x, want MNT_DETACH", target, flags)
		}
		tried = append(tried, target)
		if target == "merged" {
			return unix.EINVAL // already gone
		}
		return nil
	}
	if err := detach([]string{"merged", "lower"}, unmount); err != nil {
		t.Fatalf("a target that is not a mount failed the release: %v", err)
	}
	if len(tried) != 2 || tried[0] != "merged" || tried[1] != "lower" {
		t.Errorf("tried %v, want merged then lower", tried)
	}
}

func TestDetachReportsARealFailureAndCarriesOn(t *testing.T) {
	var tried []string
	unmount := func(target string, _ int) error {
		tried = append(tried, target)
		if target == "merged" {
			return unix.EPERM
		}
		return nil
	}
	err := detach([]string{"merged", "lower"}, unmount)
	if !errors.Is(err, unix.EPERM) {
		t.Errorf("err = %v, want EPERM", err)
	}
	if len(tried) != 2 {
		t.Errorf("tried %v, want the lower too after the union failed", tried)
	}
}
