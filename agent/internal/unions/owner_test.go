package unions

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lhns/remote-docker/core-agent/union"
)

type chowned struct {
	path     string
	uid, gid int
}

// The owner a prepare names reaches the union's spec, which gives the child the
// ids to chown the upper with before it mounts, and the agent chowns the upper
// itself once the union answers: a union ADOPTED after an agent restart was
// mounted by a child of an earlier agent and keeps whatever that one made.
func TestPrepareGivesTheUnionRootItsOwner(t *testing.T) {
	m := manager(t)
	var seen []chowned
	m.lchown = func(path string, uid, gid int) error {
		seen = append(seen, chowned{path, uid, gid})
		return nil
	}

	var starts atomic.Int64
	var spec union.Spec
	m.onStart = func(s union.Spec) { spec = s; starts.Add(1) }
	m.probe = func(context.Context, union.Spec) error {
		if starts.Load() == 0 {
			return errors.New("union: nothing is mounted there")
		}
		return nil
	}

	req := prepareFor(t, coldExport)
	req.Owner = "1000:2000"
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if spec.UID != 1000 || spec.GID != 2000 {
		t.Errorf("the child was given %d:%d, want 1000:2000", spec.UID, spec.GID)
	}
	if len(seen) != 1 || seen[0].uid != 1000 || seen[0].gid != 2000 || !strings.HasSuffix(seen[0].path, "/upper") {
		t.Fatalf("chowned %+v, want the upper given to 1000:2000", seen)
	}

	// A second prepare finds the union alive. The same owner is the same
	// union; another is refused, because the supervisor would restart the
	// child with the old one and revert it.
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	req.Owner = "5:6"
	_, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req)
	if err == nil || !strings.Contains(err.Error(), "one owner") || !strings.Contains(err.Error(), "\n  fix: ") {
		t.Fatalf("a different owner on a live union = %v, want a refusal with a fix", err)
	}
	if starts.Load() != 1 || len(seen) != 1 {
		t.Errorf("%d starts, chowned %+v; want one union, owned as it was", starts.Load(), seen)
	}
	l, _ := m.share("alice", thisClient, coldExport)
	if l.spec.UID != 1000 || l.spec.GID != 2000 {
		t.Errorf("the live spec is %d:%d, want 1000:2000", l.spec.UID, l.spec.GID)
	}
}

// A union that cannot be given its owner is discarded, not left serving: a
// registered union that answers makes every later Prepare for the share find
// it alive and be refused or skip the chown.
func TestAChownThatFailsDiscardsTheUnion(t *testing.T) {
	m := manager(t)
	fail := true
	m.lchown = func(string, int, int) error {
		if fail {
			return errors.New("operation not permitted")
		}
		return nil
	}
	var starts atomic.Int64
	m.onStart = func(union.Spec) { starts.Add(1) }
	m.probe = func(context.Context, union.Spec) error {
		if starts.Load() == 0 {
			return errors.New("union: nothing is mounted there")
		}
		return nil
	}

	req := prepareFor(t, coldExport)
	req.Owner = "1000:2000"
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req); err == nil {
		t.Fatal("Prepare succeeded although the owner could not be applied")
	}
	if _, ok := m.share("alice", thisClient, coldExport); ok {
		t.Error("the union stayed registered after its chown failed")
	}

	fail = false
	req.Owner = "5:6"
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req); err != nil {
		t.Errorf("a later Prepare with another owner = %v, want it to mount afresh", err)
	}
}

// A request that names no owner is an older client, and gets root as it always
// did.
func TestPrepareWithoutAnOwnerIsRoot(t *testing.T) {
	m := manager(t)
	var spec union.Spec
	var seen []chowned
	m.lchown = func(path string, uid, gid int) error { seen = append(seen, chowned{path, uid, gid}); return nil }
	var started atomic.Bool
	m.onStart = func(s union.Spec) { spec = s; started.Store(true) }
	m.probe = func(context.Context, union.Spec) error {
		if !started.Load() {
			return errors.New("union: nothing is mounted there")
		}
		return nil
	}
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, prepareFor(t, coldExport)); err != nil {
		t.Fatal(err)
	}
	if spec.UID != 0 || spec.GID != 0 || len(seen) != 1 || seen[0].uid != 0 || seen[0].gid != 0 {
		t.Errorf("spec %d:%d, chowned %+v; want root", spec.UID, spec.GID, seen)
	}
}

// An owner that does not parse is refused before anything is mounted.
func TestPrepareRefusesABadOwner(t *testing.T) {
	m := manager(t)
	req := prepareFor(t, coldExport)
	req.Owner = "alice"
	if _, err := m.Prepare(context.Background(), "alice", thisClient, Daemon{}, req); err == nil {
		t.Error("an owner that is not one was accepted")
	}
}
