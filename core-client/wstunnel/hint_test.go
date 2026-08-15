package wstunnel

import (
	"net/http"
	"strings"
	"testing"
)

// A hint may only name something that exists.
//
// The 404 hint used to say "check the path matches the agent's --ws-path".
// There is no such flag: the agent accepts the upgrade on any path (ADR 0034)
// and its only listener flag is --ws-addr. It was read by somebody whose
// gateway answered 404 while the agent restarted, and it sent them looking for
// a setting nothing has.
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
