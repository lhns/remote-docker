package machine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
)

// brokerExecutor is the hyperVExecutor that asks the SYSTEM service instead of
// running PowerShell: the same typed operations, one connection each. The
// backend above it is the same hyperVBackend, so no Hyper-V decision is made
// twice (ADR 0055).
type brokerExecutor struct {
	// dial opens the pipe, starting the service first if it is stopped.
	dial func(ctx context.Context) (net.Conn, error)
}

// call sends one request and reads the one answer. A failure the service
// reported is a *BrokerError.
func (e brokerExecutor) call(ctx context.Context, req BrokerRequest) (BrokerResponse, error) {
	conn, err := e.dial(ctx)
	if err != nil {
		return BrokerResponse{}, err
	}
	defer func() { _ = conn.Close() }()

	// A create can take minutes; the context is the only thing that ends it
	// early, and closing the connection is how it reaches a blocked read.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	// A service that refuses a caller answers without reading and closes, so
	// the write can fail while the answer is waiting. Read it either way.
	werr := json.NewEncoder(conn).Encode(req)
	var resp BrokerResponse
	if err := json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&resp); err != nil {
		if werr != nil {
			err = fmt.Errorf("sending to the Hyper-V service: %w", werr)
		} else if errors.Is(err, io.EOF) {
			err = errors.New("the Hyper-V service closed the connection without answering")
		}
		return BrokerResponse{}, ctxErr(ctx, err)
	}
	if !resp.OK {
		return resp, &BrokerError{Code: resp.Code, Message: resp.Error}
	}
	return resp, nil
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (e brokerExecutor) Available(ctx context.Context) error {
	_, err := e.call(ctx, BrokerRequest{Op: opAvailable})
	return err
}

func (e brokerExecutor) Look(ctx context.Context, name string) (State, hyperVNotes, error) {
	resp, err := e.call(ctx, BrokerRequest{Op: opLook, Name: name})
	if err != nil {
		return Absent, hyperVNotes{}, err
	}
	var notes hyperVNotes
	if resp.Notes != nil {
		notes = hyperVNotes{Generation: resp.Notes.Generation, Key: resp.Notes.Key}
	}
	return parseStateName(resp.State), notes, nil
}

// Create sends the settings and no disk: the service fetches Flatcar itself,
// because a path from here would be a path it opens as SYSTEM.
func (e brokerExecutor) Create(ctx context.Context, spec Spec) error {
	if spec.Rootfs != "" && !spec.Fetched {
		return errors.New("--rootfs cannot be used with the Hyper-V service, which downloads its own Flatcar disk\n" +
			"  fix: omit --rootfs, or create the machine without the service")
	}
	_, err := e.call(ctx, BrokerRequest{Op: opCreate, Name: spec.Name, Create: &BrokerCreate{
		PublicKey: spec.PublicKey, HostKey: spec.HostKey, Account: spec.Account,
		Port: spec.Port, CPUs: spec.CPUs, MemoryMB: spec.MemoryMB,
		Image: spec.Image, Proxy: spec.Proxy,
	}})
	return err
}

func (e brokerExecutor) named(ctx context.Context, op, name string) error {
	_, err := e.call(ctx, BrokerRequest{Op: op, Name: name})
	return err
}

func (e brokerExecutor) Start(ctx context.Context, name string) error {
	return e.named(ctx, opStart, name)
}
func (e brokerExecutor) Stop(ctx context.Context, name string) error {
	return e.named(ctx, opStop, name)
}
func (e brokerExecutor) Retract(ctx context.Context, name string) error {
	return e.named(ctx, opRetract, name)
}
func (e brokerExecutor) Destroy(ctx context.Context, name string) error {
	return e.named(ctx, opDestroy, name)
}

func (e brokerExecutor) Address(ctx context.Context, name string) (string, error) {
	resp, err := e.call(ctx, BrokerRequest{Op: opAddress, Name: name})
	return resp.Address, err
}

// BrokerPurge asks the service to destroy every machine it owns. Administrators
// only; the installer's uninstall runs it.
func BrokerPurge(ctx context.Context) ([]string, error) {
	resp, err := brokerExecutor{dial: dialBroker}.call(ctx, BrokerRequest{Op: opPurge})
	return resp.Purged, err
}
