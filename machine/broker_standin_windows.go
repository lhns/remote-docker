//go:build windows && rdstandin

package machine

// A stand-in for Hyper-V, compiled only with the rdstandin tag. GitHub's
// runners have no Hyper-V, so the installer's CI build uses this to run the
// real service, pipe, group and installer around a fake hypervisor. A release
// binary never contains it (msi.yml asserts that).
//
// State and the log of mutating operations live beside the service's
// directory, not in it, so teardown deleting that directory leaves the log for
// the test to read.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// standinMarker is a string a release binary must not contain.
const standinMarker = "REMOTE-DOCKER-STANDIN-HYPERV"

func standinDir() string {
	return filepath.Join(os.Getenv("ProgramData"), "remote-docker-standin")
}

func serviceExecutor(string) hyperVExecutor { return standinHyperV{dir: standinDir()} }

func serviceDisk(_ context.Context, _, _ string) (string, error) {
	dir := standinDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	disk := filepath.Join(dir, "disk.vhdx")
	return disk, os.WriteFile(disk, []byte(standinMarker), 0o644)
}

type standinVM struct {
	State string      `json:"state"`
	Port  int         `json:"port"`
	Notes hyperVNotes `json:"notes"`
}

var (
	standinMu        sync.Mutex
	standinListeners = map[int]net.Listener{}
)

type standinHyperV struct{ dir string }

func (s standinHyperV) path(name string) (string, error) {
	if err := validBrokerName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, "vms", name+".json"), nil
}

func (s standinHyperV) load(name string) (standinVM, bool, error) {
	p, err := s.path(name)
	if err != nil {
		return standinVM{}, false, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return standinVM{}, false, nil
	}
	if err != nil {
		return standinVM{}, false, err
	}
	var vm standinVM
	return vm, true, json.Unmarshal(raw, &vm)
}

func (s standinHyperV) save(name string, vm standinVM) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	raw, _ := json.Marshal(vm)
	return os.WriteFile(p, raw, 0o644)
}

// logOp records a mutating operation, one line per call.
func (s standinHyperV) logOp(op, name string) {
	_ = os.MkdirAll(s.dir, 0o755)
	f, err := os.OpenFile(filepath.Join(s.dir, "ops.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s %s\r\n", time.Now().Format(time.RFC3339), op, name)
}

func (standinHyperV) Available(context.Context) error { return nil }

func (s standinHyperV) Look(_ context.Context, name string) (State, hyperVNotes, error) {
	standinMu.Lock()
	defer standinMu.Unlock()
	vm, ok, err := s.load(name)
	if err != nil || !ok {
		return Absent, hyperVNotes{}, err
	}
	return parseStateName(vm.State), vm.Notes, nil
}

func (s standinHyperV) Create(_ context.Context, spec Spec) error {
	standinMu.Lock()
	defer standinMu.Unlock()
	s.logOp("create", spec.Name)
	return s.save(spec.Name, standinVM{State: "running", Port: spec.Port,
		Notes: hyperVNotes{Generation: spec.Generation(), Key: keyFingerprint(spec.PublicKey)}})
}

func (s standinHyperV) setState(op, name, state string) error {
	standinMu.Lock()
	defer standinMu.Unlock()
	vm, ok, err := s.load(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such machine %q", name)
	}
	if vm.State != state {
		s.logOp(op, name)
	}
	vm.State = state
	return s.save(name, vm)
}

func (s standinHyperV) Start(_ context.Context, name string) error {
	return s.setState("start", name, "running")
}

func (s standinHyperV) Stop(_ context.Context, name string) error {
	return s.setState("stop", name, "stopped")
}

func (s standinHyperV) Retract(context.Context, string) error { return nil }

// Address answers loopback and makes sure something listens on the machine's
// port in this process, which is what Locate waits for.
func (s standinHyperV) Address(_ context.Context, name string) (string, error) {
	standinMu.Lock()
	defer standinMu.Unlock()
	vm, ok, err := s.load(name)
	if err != nil || !ok || vm.State != "running" {
		return "", err
	}
	if standinListeners[vm.Port] == nil {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(vm.Port))
		if err != nil {
			return "", err
		}
		standinListeners[vm.Port] = l
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	return "127.0.0.1", nil
}

func (s standinHyperV) Destroy(_ context.Context, name string) error {
	standinMu.Lock()
	defer standinMu.Unlock()
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if _, ok, _ := s.load(name); ok {
		s.logOp("destroy", name)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
