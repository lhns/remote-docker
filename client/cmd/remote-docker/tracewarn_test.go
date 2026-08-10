package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/proxy"
)

// Silence in the three cases where the warning would be wrong.
//
// The one it exists for (this process tracing, the session not) cannot be
// exercised here: proxy.Tracing() is read from the environment once, at
// package init, which is the whole reason the mistake is possible in the first
// place. So the halves are tested for what each can prove. This one pins that
// a session already tracing is never nagged, which is the case a careless
// `if proxy.Tracing()` would get wrong.
func TestTraceWarningStaysQuietWhenItWouldBeWrong(t *testing.T) {
	var buf bytes.Buffer

	// The session IS tracing: whatever this process was told, the user is
	// already getting what they asked for.
	warnTraceGoesNowhere(&buf, proxy.Status{Tracing: true, PID: 42})
	if buf.Len() != 0 {
		t.Errorf("a tracing session was warned about anyway: %s", buf.String())
	}

	// Neither is tracing, which is the ordinary case and must print nothing:
	// output belongs to the command being run.
	if !proxy.Tracing() {
		buf.Reset()
		warnTraceGoesNowhere(&buf, proxy.Status{PID: 42})
		if buf.Len() != 0 {
			t.Errorf("a session nobody asked to trace was warned about: %s", buf.String())
		}
	}
}

// And when it does fire it has to name the variable, the process, and what to
// do -- a warning that only says "this did nothing" leaves the reader where
// they were.
func TestTraceWarningNamesTheVariableAndThePID(t *testing.T) {
	var buf bytes.Buffer
	writeTraceWarning(&buf, proxy.Status{PID: 4242})

	got := buf.String()
	for _, want := range []string{proxy.TraceEnv, "4242", "restart"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning does not mention %q:\n%s", want, got)
		}
	}
}
