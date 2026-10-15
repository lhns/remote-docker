package machine

import (
	"context"
	"strings"
	"testing"
)

// fakeHyperV records the operations the backend asks of its executor.
type fakeHyperV struct {
	calls []string
	state State
	notes hyperVNotes
}

func (f *fakeHyperV) rec(op, name string) { f.calls = append(f.calls, op+" "+name) }

func (f *fakeHyperV) Available(context.Context) error { f.rec("available", ""); return nil }
func (f *fakeHyperV) Look(_ context.Context, n string) (State, hyperVNotes, error) {
	f.rec("look", n)
	return f.state, f.notes, nil
}
func (f *fakeHyperV) Create(_ context.Context, s Spec) error  { f.rec("create", s.Name); return nil }
func (f *fakeHyperV) Start(_ context.Context, n string) error { f.rec("start", n); return nil }
func (f *fakeHyperV) Stop(_ context.Context, n string) error  { f.rec("stop", n); return nil }
func (f *fakeHyperV) Address(_ context.Context, n string) (string, error) {
	f.rec("address", n)
	return "172.20.0.2", nil
}
func (f *fakeHyperV) Retract(_ context.Context, n string) error { f.rec("retract", n); return nil }
func (f *fakeHyperV) Destroy(_ context.Context, n string) error { f.rec("destroy", n); return nil }

// Every operation the backend performs reaches the executor, with the bare
// name; the prefix is the executor's business.
func TestHyperVBackendGoesThroughItsExecutor(t *testing.T) {
	ctx := context.Background()
	f := &fakeHyperV{state: Running, notes: hyperVNotes{Generation: "g", Key: keyFingerprint("k")}}
	b := hyperVBackend{exec: f}

	_ = b.Available(ctx)
	obs, _ := b.Inspect(ctx, "dev")
	_ = b.Create(ctx, Spec{Name: "dev"})
	_ = b.Start(ctx, "dev")
	_, _ = b.Address(ctx, "dev")
	_ = b.Retract(ctx, "dev")
	_ = b.Stop(ctx, "dev")
	_ = b.Destroy(ctx, "dev")

	want := "available  look dev create dev start dev address dev retract dev stop dev destroy dev"
	if got := strings.Join(f.calls, " "); got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if obs.State != Running || obs.Generation != "g" {
		t.Errorf("Inspect = %+v", obs)
	}

	if err := b.Enrol(ctx, "dev", "dev", "other key"); err == nil {
		t.Error("a different key was enrolled on a machine built with another")
	}
	if err := b.Enrol(ctx, "dev", "dev", "k"); err != nil {
		t.Errorf("the key the machine was built with was refused: %v", err)
	}
}
