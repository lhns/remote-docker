package dircache

import (
	"errors"
	"fmt"
	"testing"
)

// The classes that do not depend on the platform.
func TestClassifyWriteWithoutAnErrno(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want fault
	}{
		{"nothing", nil, faultOther},
		{"a plain error", errors.New("the link dropped"), faultOther},
		{"a path that leaves the share", errors.New("openat ../x: path escapes from parent"), faultName},
		{"a case collision", fmt.Errorf("landing: %w", &caseClashError{path: "a/license", existing: "LICENSE"}), faultName},
		{"a disk error wrapped twice", fmt.Errorf("round: %w", fmt.Errorf("file: %w", errNoSpace)), faultSpace},
		{"a bad name wrapped", fmt.Errorf("round: %w", errBadName), faultName},
	} {
		if got := classifyWrite(tc.err); got != tc.want {
			t.Errorf("%s classified %v, want %v", tc.name, got, tc.want)
		}
	}
}
