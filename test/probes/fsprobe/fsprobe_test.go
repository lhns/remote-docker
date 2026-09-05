//go:build linux

package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// passes is how many times the transcript is produced before it is compared.
// Two was not enough: a step whose answer is a race is right most of the time,
// so a single comparison passes on the runs where the race went the same way
// twice and the failure arrives later, in a diff between two filesystems, where
// it reads as a difference between them.
const passes = 3

// The transcript is only useful if runs against the same filesystem are
// identical: labels reset per group, nothing printed names the directory, and
// every line carries an id of its own, because diff.sh keys on it.
func TestTranscriptIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	runs := make([]string, passes)
	for i := range runs {
		var out bytes.Buffer
		if err := run(dir, nil, false, &out); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		runs[i] = out.String()
	}
	for i := 1; i < len(runs); i++ {
		if runs[i] != runs[0] {
			t.Fatalf("run 0 and run %d differ:\n%s", i, diffContext(runs[0], runs[i], 3))
		}
	}
	if strings.Contains(runs[0], dir) {
		t.Fatalf("transcript names the directory:\n%s", runs[0])
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(runs[0]), "\n") {
		id, _, ok := strings.Cut(line, ": ")
		if !ok || !strings.Contains(line, " -> ") {
			t.Errorf("malformed line: %q", line)
			continue
		}
		if seen[id] {
			t.Errorf("step id printed twice: %s", id)
		}
		seen[id] = true
		if strings.Contains(line, "PANIC:") {
			t.Errorf("step panicked: %s", line)
		}
	}
}

// diffContext renders only the lines where a and b differ, with ctx lines of
// context around each run of them. The whole transcript is several hundred
// lines and CI truncates it, which is how the first failure of this test
// arrived with the differing line cut off; the line and the step id are the
// only things worth printing.
func diffContext(a, b string, ctx int) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	n := max(len(al), len(bl))
	at := func(lines []string, i int) string {
		if i < len(lines) {
			return lines[i]
		}
		return "<no line>"
	}

	show := make([]bool, n)
	differs := 0
	for i := range n {
		if at(al, i) != at(bl, i) {
			differs++
			for j := max(i-ctx, 0); j < min(i+ctx+1, n); j++ {
				show[j] = true
			}
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "%d of %d lines differ\n", differs, n)
	gap := false
	for i := range n {
		if !show[i] {
			gap = true
			continue
		}
		if gap {
			out.WriteString("  ...\n")
			gap = false
		}
		if at(al, i) == at(bl, i) {
			fmt.Fprintf(&out, "  %s\n", at(al, i))
			continue
		}
		fmt.Fprintf(&out, "- %s\n+ %s\n", at(al, i), at(bl, i))
	}
	return out.String()
}

func TestUnknownGroup(t *testing.T) {
	if err := run(t.TempDir(), []string{"nope"}, false, &bytes.Buffer{}); err == nil {
		t.Fatal("unknown group accepted")
	}
}
