package main

// Docker's rule: a command that takes a session away from what depends on it
// refuses without -f, and one that has nothing depending on it just acts.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
	"github.com/lhns/remote-docker/machine"
)

// events is what the fakes below were asked to do, in order.
type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *events) String() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.list, ",")
}

// fakeSession serves the session's control channel at a fresh endpoint,
// answering idle with safe, and stopping serving when told to shut down.
func fakeSession(t *testing.T, safe bool, log *events) string {
	t.Helper()
	endpoint := filepath.Join(t.TempDir(), "s.sock")
	if runtime.GOOS == "windows" {
		endpoint = `\\.\pipe\remote-docker-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	}
	l, err := proxy.Listen(endpoint)
	if err != nil {
		t.Skipf("cannot bind a test endpoint here: %v", err)
	}
	mux := http.NewServeMux()
	// The server closes the listener, and only once: Serve closes it on return
	// too, and two closes of the endpoint's listener race.
	srv := &http.Server{Handler: mux}
	stop := func() { _ = srv.Close() }
	t.Cleanup(stop)

	mux.HandleFunc(proxy.ControlPrefix+"idle", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(proxy.Idle{Safe: safe})
	})
	mux.HandleFunc(proxy.ControlPrefix+"status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(proxy.Status{})
	})
	mux.HandleFunc(proxy.ControlPrefix+"shutdown", func(w http.ResponseWriter, _ *http.Request) {
		log.add("shutdown")
		w.WriteHeader(http.StatusOK)
		go stop()
	})
	go func() { _ = srv.Serve(l) }()
	return endpoint
}

// fakeBackend is a machine backend that records what it is asked.
type fakeBackend struct {
	log   *events
	state machine.State
}

func (b *fakeBackend) Name() string                    { return "fake" }
func (b *fakeBackend) Available(context.Context) error { return nil }
func (b *fakeBackend) Inspect(_ context.Context, name string) (machine.Observed, error) {
	b.log.add("inspect " + name)
	return machine.Observed{State: b.state}, nil
}
func (b *fakeBackend) Create(context.Context, machine.Spec) error { b.log.add("create"); return nil }
func (b *fakeBackend) Enrol(context.Context, string, string, string) error {
	return nil
}
func (b *fakeBackend) Start(context.Context, string) error { b.log.add("start"); return nil }
func (b *fakeBackend) Hold(context.Context, string) (io.Closer, error) {
	return nil, errors.New("fakeBackend: no hold")
}
func (b *fakeBackend) Address(context.Context, string) (string, error) { return "", nil }
func (b *fakeBackend) Stop(_ context.Context, name string) error {
	b.log.add("stop " + name)
	return nil
}
func (b *fakeBackend) Destroy(_ context.Context, name string) error {
	b.log.add("destroy " + name)
	return nil
}

// withBackend makes every machine command find b.
func withBackend(t *testing.T, b machine.Backend) {
	t.Helper()
	saved := findBackend
	findBackend = func(string) (machine.Backend, error) { return b, nil }
	t.Cleanup(func() { findBackend = saved })
}

// oneWorkspace configures "dev" served at endpoint, backed by a fake machine
// when withMachine is set.
func oneWorkspace(t *testing.T, endpoint string, withMachine bool) {
	t.Helper()
	ws := config.Workspace{Host: "127.0.0.1", Port: 1, Endpoint: endpoint}
	if withMachine {
		ws.Machine = &config.Machine{Backend: "fake", Name: "dev"}
	}
	withConfig(t, &config.File{Default: "dev", Workspaces: map[string]config.Workspace{"dev": ws}})
}

func TestInUseRefusesWithoutForce(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		machine bool
		fix     string
	}{
		{[]string{"stop"}, false, "remote stop dev -f"},
		{[]string{"restart"}, false, "remote restart dev -f"},
		{[]string{"machine", "stop"}, true, "remote machine stop dev -f"},
		{[]string{"machine", "rebuild", "dev"}, true, "remote machine rebuild dev -f"},
		{[]string{"rm", "dev"}, true, "remote rm dev -f"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			log := &events{}
			oneWorkspace(t, fakeSession(t, false, log), tc.machine)
			withBackend(t, &fakeBackend{log: log})

			err := run(t, append([]string{"remote"}, tc.args...)...)
			requireFixLine(t, err, tc.fix)
			if !strings.Contains(err.Error(), `the session for "dev" is in use`) {
				t.Errorf("the refusal does not name what is in use: %v", err)
			}
			if log.String() != "" {
				t.Errorf("a refused command still did %s", log)
			}
		})
	}
}

// -f goes ahead, and the session is stopped before the machine is touched.
func TestForceStopsTheSessionFirst(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
		out  string
	}{
		{[]string{"stop", "-f"}, "shutdown", "stopped: "},
		{[]string{"machine", "stop", "--force"}, "shutdown,stop dev", `stopped "dev"`},
		{[]string{"rm", "dev", "-f"}, "shutdown,destroy dev", `removed workspace "dev"`},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			log := &events{}
			oneWorkspace(t, fakeSession(t, false, log), true)
			withBackend(t, &fakeBackend{log: log})

			out, err := runOut(t, append([]string{"remote"}, tc.args...)...)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if log.String() != tc.want {
				t.Errorf("did %q, want %q", log, tc.want)
			}
			if !strings.Contains(out, tc.out) {
				t.Errorf("printed %q, want %q in it", out, tc.out)
			}
		})
	}
}

// Nothing depending on the session needs no -f, and rm of an ordinary
// workspace stops its session too.
func TestAnIdleSessionNeedsNoForce(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		machine bool
		want    string
	}{
		{[]string{"stop"}, false, "shutdown"},
		{[]string{"rm", "dev"}, false, "shutdown"},
		{[]string{"machine", "stop", "dev"}, true, "shutdown,stop dev"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			log := &events{}
			oneWorkspace(t, fakeSession(t, true, log), tc.machine)
			withBackend(t, &fakeBackend{log: log})

			if err := run(t, append([]string{"remote"}, tc.args...)...); err != nil {
				t.Fatal(err)
			}
			if log.String() != tc.want {
				t.Errorf("did %q, want %q", log, tc.want)
			}
		})
	}
}

func TestStatusExitsOneWhenNotReady(t *testing.T) {
	oneWorkspace(t, unreachableEndpoint(t), false)
	out, err := runOut(t, "remote", "status")
	if exitCode(err) != 1 || err.Error() != "" {
		t.Errorf("status of an unreachable workspace returned %v (exit %d), want a silent 1", err, exitCode(err))
	}
	if !strings.Contains(out, "cannot reach the workspace") {
		t.Errorf("the verdict is missing:\n%s", out)
	}
}

func TestMachineStatusExitsOneWhenNotRunning(t *testing.T) {
	for state, want := range map[machine.State]int{machine.Running: 0, machine.Stopped: 1, machine.Absent: 1} {
		t.Run(state.String(), func(t *testing.T) {
			oneWorkspace(t, unreachableEndpoint(t), true)
			withBackend(t, &fakeBackend{log: &events{}, state: state})
			out, err := runOut(t, "remote", "machine", "status")
			if exitCode(err) != want {
				t.Errorf("exit %d (%v), want %d", exitCode(err), err, want)
			}
			if !strings.Contains(out, state.String()) {
				t.Errorf("the state is missing:\n%s", out)
			}
		})
	}
}

// A machine command takes its [name], else --workspace, else the default.
func TestMachineCommandsSelectAWorkspace(t *testing.T) {
	m := func(name string) *config.Machine { return &config.Machine{Backend: "fake", Name: name} }
	withConfig(t, &config.File{
		Default: "main",
		Workspaces: map[string]config.Workspace{
			"main":  {Host: "127.0.0.1", Machine: m("main")},
			"other": {Host: "127.0.0.1", Machine: m("other")},
		},
	})

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"remote", "machine", "status"}, "inspect main"},
		{[]string{"remote", "--workspace", "other", "machine", "status"}, "inspect other"},
		{[]string{"remote", "--workspace", "other", "machine", "status", "main"}, "inspect main"},
	} {
		t.Run(strings.Join(tc.args[1:], " "), func(t *testing.T) {
			log := &events{}
			withBackend(t, &fakeBackend{log: log, state: machine.Running})
			if err := run(t, tc.args...); err != nil {
				t.Fatal(err)
			}
			if log.String() != tc.want {
				t.Errorf("asked %q, want %q", log, tc.want)
			}
		})
	}
}

// The endpoint a named workspace is served on keeps the overrides and replaces
// only the workspace, so `machine stop other` never reaches the default's.
func TestResolveNamesTheWorkspaceAsked(t *testing.T) {
	withConfig(t, &config.File{
		Default: "main",
		Workspaces: map[string]config.Workspace{
			"main":  {Host: "main.example", Endpoint: "/tmp/main.sock"},
			"other": {Host: "other.example", Endpoint: "/tmp/other.sock"},
		},
	})
	cfg, err := resolve([]string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if got := endpointOf(cfg); got != "/tmp/other.sock" {
		t.Errorf("endpoint = %q, want /tmp/other.sock and not the default workspace's", got)
	}
}
