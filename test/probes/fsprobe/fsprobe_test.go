//go:build linux

package main

import (
	"bytes"
	"strings"
	"testing"
)

// The transcript is only useful if two runs against the same filesystem are
// identical: labels reset per run, and nothing printed names the directory.
func TestTranscriptIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	var runs [2]string
	for i := range runs {
		var out bytes.Buffer
		if err := run(dir, nil, false, &out); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		runs[i] = out.String()
	}
	if runs[0] != runs[1] {
		t.Fatalf("two runs differ:\n--- first\n%s\n--- second\n%s", runs[0], runs[1])
	}
	if strings.Contains(runs[0], dir) {
		t.Fatalf("transcript names the directory:\n%s", runs[0])
	}
	for _, line := range strings.Split(strings.TrimSpace(runs[0]), "\n") {
		if strings.Contains(line, "PANIC:") {
			t.Errorf("step panicked: %s", line)
		}
		if !strings.Contains(line, ": ") || !strings.Contains(line, " -> ") {
			t.Errorf("malformed line: %q", line)
		}
	}
}

func TestUnknownGroup(t *testing.T) {
	if err := run(t.TempDir(), []string{"nope"}, false, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown group accepted")
	}
}
