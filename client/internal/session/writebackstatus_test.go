package session

import (
	"strings"
	"testing"
	"time"

	"github.com/lhns/remote-docker/dircache"
)

// Every share that carries writes back says whether it is moving, and a stall
// names why, where, how many and when it tries next. Nothing is left behind
// without being named.
func TestWriteBackStatusNamesWhatIsStalledAndWhatWasLeftBehind(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 30, 0, 0, time.UTC)
	reports := []dircache.Report{
		{Local: "/home/alice/b-ephemeral", Ephemeral: true},
		{Local: "/home/alice/c-stalled", WriteBack: dircache.WriteBackReport{
			Stalled: true, Since: at, Next: at.Add(time.Minute), Pending: 7,
			Reason: "writing back what a container wrote: no space left on device", FirstPath: "/dist/app.js",
		}},
		{Local: "/home/alice/a-fine", WriteBack: dircache.WriteBackReport{Skipped: []dircache.Skipped{
			{Path: "/bad?name", Why: "invalid name"},
			{Path: "/node_modules/.bin/tsc", Why: "privilege not held", Kind: dircache.SkipLink},
			{Path: "/node_modules/.bin/tsserver", Why: "privilege not held", Kind: dircache.SkipLink},
			{Path: "/license", Why: "differs only in case", Kind: dircache.SkipClash},
			{Path: "/x/.wh..wh..opq", Why: "an overlay marker, never a file of yours", Kind: dircache.SkipMarker},
			{Path: "/out.txt", Why: "you changed it here after the container did; your version is kept", Kind: dircache.SkipKept},
			{Path: "/index.json", Why: "you changed it here after the container did; your version is kept", Kind: dircache.SkipKept},
		}}},
	}

	out := strings.Join(writeBackLines(reports, workspaceSays{known: true, carries: true, times: true}), "\n")

	for _, want := range []string{
		"a-fine: ok",
		"c-stalled: stalled since 12:30:00, writing back what a container wrote: no space left on device",
		"7 change(s) pending; first: dist/app.js; next try 12:31:00",
		"1 file(s) this machine cannot hold are left until they change: bad?name",
		"2 link(s) not carried back, which this account may not create here: node_modules/.bin/tsc, node_modules/.bin/tsserver",
		"1 file(s) differ only in case from one already here and are left: license",
		"1 overlay marker(s) in the tree, safe to delete: x/.wh..wh..opq",
		"2 container change(s) not carried back, as you changed it here after the container did; your version is kept: out.txt, index.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ephemeral") {
		t.Errorf("a share that discards writes is listed:\n%s", out)
	}
	if strings.Index(out, "a-fine: ok") > strings.Index(out, "c-stalled") {
		t.Errorf("shares are not in path order:\n%s", out)
	}
}

// A workspace that cannot carry links and replaced directories back says so, once,
// with the remedy, and only when the channel is open and said it.
func TestWriteBackStatusSaysWhenTheWorkspaceCannotCarryLinks(t *testing.T) {
	reports := func() []dircache.Report { return []dircache.Report{{Local: "/home/alice/p"}} }

	if out := strings.Join(writeBackLines(reports(), workspaceSays{known: true, times: true}), "\n"); !strings.Contains(out, "too old to carry links") ||
		!strings.Contains(out, "fix: update the workspace") {
		t.Errorf("an agent without Carry: %q", out)
	}
	// Without change times a deletion of a file the cache never held is kept
	// (ADR 0044), and status says the remedy.
	if out := strings.Join(writeBackLines(reports(), workspaceSays{known: true, carries: true}), "\n"); !strings.Contains(out, "too old to say when a container deleted a file") ||
		!strings.Contains(out, "fix: update the workspace") {
		t.Errorf("an agent without Times: %q", out)
	}
	for _, says := range []workspaceSays{{known: true, carries: true, times: true}, {}} {
		if out := strings.Join(writeBackLines(reports(), says), "\n"); strings.Contains(out, "too old") {
			t.Errorf("%+v said the workspace is too old: %q", says, out)
		}
	}
	if out := writeBackLines(nil, workspaceSays{known: true}); len(out) != 0 {
		t.Errorf("no share carries writes back, yet status says %v", out)
	}
}
