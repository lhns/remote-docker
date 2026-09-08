package daemons

import (
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
