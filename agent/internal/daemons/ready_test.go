package daemons

import (
	"errors"
	"testing"
	"time"
)

// The budget is a setting with a default, and the zero value has to mean the
// default rather than "no time at all": a Manager built without one is what
// every caller but serve constructs.
func TestTheReadyBudgetDefaultsAndIsOverridable(t *testing.T) {
	if got := (&Manager{}).readyTimeout(); got != DefaultReadyTimeout {
		t.Errorf("an unset budget is %s, want %s", got, DefaultReadyTimeout)
	}
	if got := (&Manager{ReadyTimeout: 20 * time.Second}).readyTimeout(); got != 20*time.Second {
		t.Errorf("the configured budget did not take: %s", got)
	}
}

// A failed start is remembered for a moment, so the callers behind it fail
// fast rather than each becoming the next leader and paying the whole ready
// budget again (see failTTL).
//
// The account name is refused by Plan, which makes start fail without touching
// the filesystem. What is asserted is the error's IDENTITY: the same value back
// means it was answered from the record, a fresh one that the attempt ran again.
func TestAFailedStartIsRememberedSoTheNextCallerDoesNotPayForItAgain(t *testing.T) {
	m := manager(fakeDocker{})

	first, err := m.ensure(t.Context(), "not a name")
	if err == nil {
		t.Fatalf("ensure accepted an unusable account name: %+v", first)
	}

	_, again := m.ensure(t.Context(), "not a name")
	if again != err { //nolint:errorlint // identity is the point: a new error means a second attempt
		t.Errorf("the second caller started over: %v, want the recorded %v", again, err)
	}

	if f, ok := m.failed["not a name"]; !ok || f.err != err {
		t.Errorf("the failure was not recorded: %+v", m.failed)
	}
}

// The record must not outlive the repair. `remote-dockerd daemons reset` is
// exactly what somebody runs on a daemon that will not start, so the next
// request has to try rather than be told about the last failure.
func TestResetForgetsTheRecordedFailure(t *testing.T) {
	m := manager(fakeDocker{})
	m.failed = map[string]failure{"alice": {at: time.Now(), err: errors.New("it would not start")}}

	if err := m.Reset(t.Context(), "alice", false); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, ok := m.failed["alice"]; ok {
		t.Error("Reset left the last failure behind, so the next caller is told about it")
	}
}
