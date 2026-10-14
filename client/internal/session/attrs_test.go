package session

import "testing"

func TestDefaultAttrsOwner(t *testing.T) {
	if a := defaultAttrs(0, 0); a.UID != 0 || a.GID != 0 {
		t.Errorf("default owner = %d:%d, want 0:0", a.UID, a.GID)
	}
	if a := defaultAttrs(1000, 100); a.UID != 1000 || a.GID != 100 {
		t.Errorf("owner = %d:%d, want 1000:100", a.UID, a.GID)
	}
}
