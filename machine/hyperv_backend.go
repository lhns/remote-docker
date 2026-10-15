package machine

import (
	"context"
	"io"
)

// hyperVExecutor is every operation the Hyper-V backend performs on the host,
// as typed calls. The backend decides; an executor only does.
//
// Typed rather than "run this script" so that an implementation which does not
// run in this process can carry them: whoever answers is handed an operation,
// a machine name and typed arguments, never PowerShell. The one implementation
// today is directHyperV, which runs PowerShell here (hyperv_windows.go).
//
// Names are the bare workspace names; the executor adds the "rd-" prefix.
type hyperVExecutor interface {
	// Available reports why Hyper-V cannot be managed from here, or nil.
	Available(ctx context.Context) error

	// Look reads a machine's state and recorded notes in one call; a machine
	// that is not there is Absent.
	Look(ctx context.Context, name string) (State, hyperVNotes, error)

	// Create builds a machine from spec and starts it. spec.Rootfs is a local
	// Flatcar disk; the executor copies it, never uses it in place.
	Create(ctx context.Context, spec Spec) error

	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error

	// Address is what the guest reported, "" while it has none.
	Address(ctx context.Context, name string) (string, error)

	// Retract removes the Ignition KVP items, private host key included.
	Retract(ctx context.Context, name string) error

	// Destroy removes the machine and its disk; a machine already gone is not
	// an error.
	Destroy(ctx context.Context, name string) error
}

// hyperVBackend is the Hyper-V Backend, over an executor. It holds no platform
// code, so it is tested everywhere with a fake.
type hyperVBackend struct{ exec hyperVExecutor }

func (hyperVBackend) Name() string { return "hyperv" }

func (b hyperVBackend) Available(ctx context.Context) error { return b.exec.Available(ctx) }

func (b hyperVBackend) Inspect(ctx context.Context, name string) (Observed, error) {
	return observeVM(b.exec.Look(ctx, name))
}

// Create builds the machine from a Flatcar disk and one Ignition document
// (hyperVExecutor.Create).
func (b hyperVBackend) Create(ctx context.Context, spec Spec) error { return b.exec.Create(ctx, spec) }

// Retract removes the Ignition document's KVP items, private host key
// included. Removing items already gone succeeds.
func (b hyperVBackend) Retract(ctx context.Context, name string) error {
	return b.exec.Retract(ctx, name)
}

// Enrol reports whether this key already reaches the machine. It cannot write
// one: see hyperVEnrolment.
func (b hyperVBackend) Enrol(ctx context.Context, name, _, publicKey string) error {
	_, notes, err := b.exec.Look(ctx, name)
	if err != nil {
		return err
	}
	return hyperVEnrolment(notes, publicKey)
}

func (b hyperVBackend) Start(ctx context.Context, name string) error {
	return b.exec.Start(ctx, name)
}

// Hold is nothing. A Hyper-V machine runs until it is stopped: unlike a WSL
// distribution it has no idle timeout, so there is nothing to hold open.
func (hyperVBackend) Hold(context.Context, string) (io.Closer, error) {
	return closerFunc(func() error { return nil }), nil
}

func (b hyperVBackend) Address(ctx context.Context, name string) (string, error) {
	return b.exec.Address(ctx, name)
}

func (b hyperVBackend) Stop(ctx context.Context, name string) error {
	return b.exec.Stop(ctx, name)
}

func (b hyperVBackend) Destroy(ctx context.Context, name string) error {
	return b.exec.Destroy(ctx, name)
}
