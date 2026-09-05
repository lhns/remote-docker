package tunnelclient

import (
	"net/http"
	"strings"
	"testing"
)

// A hint may only name a flag that exists: the agent has --ws-addr and no
// --ws-path (ADR 0034).
func TestHintsNameOnlyFlagsThatExist(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable} {
		got := hint(&http.Response{StatusCode: code})
		if got == "" {
			t.Errorf("%d has no hint", code)
		}
		if strings.Contains(got, "--ws-path") {
			t.Errorf("%d names --ws-path, which no agent flag matches: %q", code, got)
		}
	}
}

// A status nobody can act on gets no advice at all, rather than a guess.
func TestNoHintForAnythingElse(t *testing.T) {
	if got := hint(&http.Response{StatusCode: http.StatusForbidden}); got != "" {
		t.Errorf("403 produced %q", got)
	}
	if got := hint(nil); got != "" {
		t.Errorf("no response produced %q", got)
	}
}
