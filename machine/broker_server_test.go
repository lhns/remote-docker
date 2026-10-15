package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	sidAlice  = "S-1-5-21-1-2-3-1001"
	sidBob    = "S-1-5-21-1-2-3-1002"
	sidSystem = "S-1-5-18"
)

// brokerFake is the service's direct executor, standing in for Hyper-V.
type brokerFake struct {
	mu        sync.Mutex
	calls     []string
	vms       map[string]State
	createErr error

	// block makes Look wait for it, or for its context: a Hyper-V call that
	// hangs. ctxDone receives when a blocked Look's context ended.
	block      chan struct{}
	ctxDone    chan struct{}
	destroyErr error
	onDestroy  func()
}

func newBrokerFake() *brokerFake { return &brokerFake{vms: map[string]State{}} }

func (f *brokerFake) rec(op, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op+" "+name)
}
func (f *brokerFake) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, ",")
}
func (f *brokerFake) Available(context.Context) error { f.rec("available", ""); return nil }
func (f *brokerFake) Look(ctx context.Context, n string) (State, hyperVNotes, error) {
	f.rec("look", n)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			if f.ctxDone != nil {
				f.ctxDone <- struct{}{}
			}
			return Absent, hyperVNotes{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vms[n], hyperVNotes{Generation: "g", Key: keyFingerprint("k")}, nil
}
func (f *brokerFake) Create(_ context.Context, s Spec) error {
	f.rec("create", s.Name+" "+s.Rootfs)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.vms[s.Name] = Running
	return nil
}
func (f *brokerFake) Start(_ context.Context, n string) error { f.rec("start", n); return nil }
func (f *brokerFake) Stop(_ context.Context, n string) error  { f.rec("stop", n); return nil }
func (f *brokerFake) Address(_ context.Context, n string) (string, error) {
	f.rec("address", n)
	return "172.20.0.9", nil
}
func (f *brokerFake) Retract(_ context.Context, n string) error { f.rec("retract", n); return nil }
func (f *brokerFake) Destroy(_ context.Context, n string) error {
	f.rec("destroy", n)
	if f.onDestroy != nil {
		f.onDestroy()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyErr != nil {
		return f.destroyErr
	}
	delete(f.vms, n)
	return nil
}

// brokerHarness is a server over loopback TCP, which buffers like a pipe does.
type brokerHarness struct {
	t       *testing.T
	fake    *brokerFake
	srv     *brokerServer
	mu      sync.Mutex
	caller  brokerCaller
	members []string
	addr    string
}

func newBrokerHarness(t *testing.T) *brokerHarness {
	t.Helper()
	h := &brokerHarness{t: t, fake: newBrokerFake(), caller: brokerCaller{SID: sidAlice}}
	h.srv = newBrokerServer(h.fake, filepath.Join(t.TempDir(), "machines.json"),
		func(net.Conn) (brokerCaller, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.caller, nil
		},
		func() ([]string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return slices.Clone(h.members), nil
		},
		func(context.Context, string) (string, error) { return `C:\cache\flatcar.vhdx`, nil })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.srv.Serve(ctx, drainingListener{l}) }()
	t.Cleanup(func() { cancel(); _ = l.Close(); <-done })
	h.addr = l.Addr().String()
	return h
}

// drainingListener hands out connections that read what is left before they
// close. Over TCP, closing with unread input resets the connection and the
// client loses the refusal it was about to read; a named pipe does not do that.
// The service's own code never reads on that path (failConn pins it).
type drainingListener struct{ net.Listener }

func (l drainingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	return drainingConn{c}, err
}

type drainingConn struct{ net.Conn }

func (c drainingConn) Close() error {
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _ = io.Copy(io.Discard, c.Conn)
	return c.Conn.Close()
}

// as sets who the next connections are from.
func (h *brokerHarness) as(sid string, admin bool, members ...string) *brokerExecutor {
	h.mu.Lock()
	h.caller, h.members = brokerCaller{SID: sid, Admin: admin}, members
	h.mu.Unlock()
	return &brokerExecutor{dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", h.addr)
	}}
}

func sampleCreate() BrokerCreate {
	return BrokerCreate{
		PublicKey: "ssh-ed25519 AAAAC3Nza alice@example", HostKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n",
		Account: "alice", Port: 2222, CPUs: 2, MemoryMB: 4096,
		Image: "ghcr.io/lhns/remote-docker-workspace:1.0.0", Proxy: "http://proxy.example:3128",
	}
}

func sampleSpec(name string) Spec {
	c := sampleCreate()
	return Spec{Name: name, Backend: "hyperv", Image: c.Image, Fetched: true, CPUs: c.CPUs, MemoryMB: c.MemoryMB,
		Port: c.Port, Account: c.Account, PublicKey: c.PublicKey, HostKey: c.HostKey, Proxy: c.Proxy}
}

func brokerCode(t *testing.T, err error) string {
	t.Helper()
	var be *BrokerError
	if !errors.As(err, &be) {
		t.Fatalf("error %v is not a BrokerError", err)
	}
	return be.Code
}

func TestBrokerAuthorisation(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)

	for _, c := range []struct {
		name    string
		sid     string
		admin   bool
		members []string
		ok      bool
	}{
		{"SYSTEM", sidSystem, true, nil, true},
		{"administrator", sidBob, true, nil, true},
		{"live member", sidAlice, false, []string{sidAlice}, true},
		{"member, SID in another case", sidAlice, false, []string{strings.ToLower(sidAlice)}, true},
		{"non-member", sidBob, false, []string{sidAlice}, false},
		{"empty group", sidBob, false, nil, false},
	} {
		err := h.as(c.sid, c.admin, c.members...).Available(ctx)
		if c.ok && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok {
			if err == nil || brokerCode(t, err) != codeNotAllowed {
				t.Errorf("%s: err = %v, want not-allowed", c.name, err)
			}
		}
	}

	// Added to the group after the service started: allowed on the next
	// connection, with no new logon, because the list is read every time.
	if err := h.as(sidBob, false).Available(ctx); err == nil {
		t.Fatal("bob allowed before being added")
	}
	if err := h.as(sidBob, false, sidBob).Available(ctx); err != nil {
		t.Errorf("bob refused after being added: %v", err)
	}
}

func TestBrokerFailsClosedWhenTheGroupCannotBeRead(t *testing.T) {
	h := newBrokerHarness(t)
	h.srv.members = func() ([]string, error) { return nil, errors.New("netapi down") }
	if err := h.as(sidAlice, false).Available(context.Background()); err == nil || brokerCode(t, err) != codeNotAllowed {
		t.Errorf("err = %v, want not-allowed", err)
	}
	if err := h.as(sidSystem, true).Available(context.Background()); err != nil {
		t.Errorf("an administrator was refused: %v", err)
	}
}

// failReader fails the test if anything reads from it.
type failConn struct {
	net.Conn
	t      *testing.T
	wrote  strings.Builder
	closed bool
}

func (c *failConn) Read([]byte) (int, error) {
	c.t.Error("the service read from an unauthorised caller")
	return 0, errors.New("read")
}
func (c *failConn) Write(p []byte) (int, error)      { return c.wrote.Write(p) }
func (c *failConn) Close() error                     { c.closed = true; return nil }
func (c *failConn) SetWriteDeadline(time.Time) error { return nil }
func (c *failConn) SetReadDeadline(time.Time) error {
	c.t.Error("a read deadline was set before authorising")
	return nil
}

func TestBrokerNeverReadsForAnUnauthorisedCaller(t *testing.T) {
	h := newBrokerHarness(t)
	h.as(sidBob, false) // not a member

	conn := &failConn{t: t}
	h.srv.serveConn(context.Background(), conn)

	if !strings.Contains(conn.wrote.String(), codeNotAllowed) || !conn.closed {
		t.Errorf("wrote %q, closed %v", conn.wrote.String(), conn.closed)
	}
	if h.fake.got() != "" {
		t.Errorf("an operation ran: %s", h.fake.got())
	}
}

func TestBrokerNeverReadsWhenTheCallerCannotBeIdentified(t *testing.T) {
	h := newBrokerHarness(t)
	h.srv.identify = func(net.Conn) (brokerCaller, error) { return brokerCaller{}, errors.New("no token") }
	conn := &failConn{t: t}
	h.srv.serveConn(context.Background(), conn)
	if !strings.Contains(conn.wrote.String(), codeNotAllowed) {
		t.Errorf("wrote %q", conn.wrote.String())
	}
}

func TestBrokerOwnership(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	alice := h.as(sidAlice, false, sidAlice, sidBob)

	if err := alice.Create(ctx, sampleSpec("dev")); err != nil {
		t.Fatal(err)
	}
	if got := h.fake.got(); !strings.Contains(got, `create dev C:\cache\flatcar.vhdx`) {
		t.Errorf("calls = %s: the service must build from the disk it fetched", got)
	}

	bob := h.as(sidBob, false, sidAlice, sidBob)
	h.fake.calls = nil
	for name, op := range map[string]func() error{
		"start":   func() error { return bob.Start(ctx, "dev") },
		"stop":    func() error { return bob.Stop(ctx, "dev") },
		"retract": func() error { return bob.Retract(ctx, "dev") },
		"destroy": func() error { return bob.Destroy(ctx, "dev") },
		"address": func() error { _, err := bob.Address(ctx, "dev"); return err },
		"look":    func() error { _, _, err := bob.Look(ctx, "dev"); return err },
	} {
		if err := op(); err == nil || brokerCode(t, err) != codeNotYours {
			t.Errorf("bob's %s of alice's machine: %v", name, err)
		}
	}
	if got := h.fake.got(); strings.Contains(got, "start") || strings.Contains(got, "destroy") || strings.Contains(got, "stop") {
		t.Errorf("an operation reached Hyper-V for the wrong owner: %s", got)
	}

	// The name is taken, whoever asks.
	for who, ex := range map[string]*brokerExecutor{"bob": bob, "alice": h.as(sidAlice, false, sidAlice)} {
		err := ex.Create(ctx, sampleSpec("dev"))
		if err == nil || brokerCode(t, err) != codeTaken {
			t.Errorf("%s created a taken name: %v", who, err)
		}
	}

	// Hers still.
	alice = h.as(sidAlice, false, sidAlice)
	if err := alice.Start(ctx, "dev"); err != nil {
		t.Error(err)
	}
	if addr, err := alice.Address(ctx, "dev"); err != nil || addr != "172.20.0.9" {
		t.Errorf("address = %q, %v", addr, err)
	}
	if err := alice.Destroy(ctx, "dev"); err != nil {
		t.Error(err)
	}
	// And now free for bob.
	if err := h.as(sidBob, false, sidBob).Create(ctx, sampleSpec("dev")); err != nil {
		t.Errorf("bob could not take a freed name: %v", err)
	}
}

func TestBrokerRefusesAMachineHyperVHasThatIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	h.fake.vms["theirs"] = Running // somebody's own VM called rd-theirs
	alice := h.as(sidAlice, false, sidAlice)

	if err := alice.Create(ctx, sampleSpec("theirs")); err == nil || brokerCode(t, err) != codeTaken {
		t.Errorf("create over an unmanaged machine: %v", err)
	}
	if err := alice.Destroy(ctx, "theirs"); err == nil || brokerCode(t, err) != codeNotYours {
		t.Errorf("destroy of an unmanaged machine: %v", err)
	}
	if strings.Contains(h.fake.got(), "destroy") {
		t.Error("an unmanaged machine was destroyed")
	}

	// A name nothing has is absent, destroying it is a success, starting it is not.
	if st, _, err := alice.Look(ctx, "none"); err != nil || st != Absent {
		t.Errorf("look = %v, %v", st, err)
	}
	if err := alice.Destroy(ctx, "none"); err != nil {
		t.Errorf("destroy of nothing: %v", err)
	}
	if err := alice.Start(ctx, "none"); err == nil {
		t.Error("started a machine that does not exist")
	}
}

func TestBrokerRefusesMalformedNames(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	alice := h.as(sidAlice, false, sidAlice)

	for _, name := range []string{"", "Dev", "-dev", "dev_1", "a/b", `..\x`, "dev.vhdx", "d e", "con", "nul", "com1", "lpt9", "aux", "prn", strings.Repeat("a", 64), "rd-x\n", `c:\x`} {
		if err := alice.Start(ctx, name); err == nil || brokerCode(t, err) != codeInvalid {
			t.Errorf("start %q: %v", name, err)
		}
		if err := alice.Create(ctx, sampleSpec(name)); err == nil || brokerCode(t, err) != codeInvalid {
			t.Errorf("create %q: %v", name, err)
		}
	}
	for _, name := range []string{"a", "dev", "my-ws-2", strings.Repeat("a", 63)} {
		if err := validBrokerName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
	if got := h.fake.got(); got != "" {
		t.Errorf("Hyper-V was asked about a bad name: %s", got)
	}
}

func TestBrokerRefusesBadCreateSettings(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	alice := h.as(sidAlice, false, sidAlice)

	for name, mut := range map[string]func(*Spec){
		"account with a newline": func(s *Spec) { s.Account = "a\nb" },
		"uppercase account":      func(s *Spec) { s.Account = "Alice" },
		"image with a space":     func(s *Spec) { s.Image = "x --privileged=false y" },
		"image with a newline":   func(s *Spec) { s.Image = "x\nExecStart=/bin/sh" },
		"port 0":                 func(s *Spec) { s.Port = 0 },
		"port too big":           func(s *Spec) { s.Port = 70000 },
		"key with a newline":     func(s *Spec) { s.PublicKey = "ssh-ed25519 AAAA a\nssh-ed25519 BBBB b" },
		"key that is no key":     func(s *Spec) { s.PublicKey = "hello" },
		"host key not PEM":       func(s *Spec) { s.HostKey = "/etc/shadow" },
		"proxy with quotes":      func(s *Spec) { s.Proxy = `http://x"y:1` },
		"negative cpus":          func(s *Spec) { s.CPUs = -1 },
	} {
		spec := sampleSpec("dev")
		mut(&spec)
		if err := alice.Create(ctx, spec); err == nil || brokerCode(t, err) != codeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := h.fake.got(); got != "" {
		t.Errorf("Hyper-V was asked: %s", got)
	}
}

func TestBrokerRefusesUnknownOpsAndFields(t *testing.T) {
	h := newBrokerHarness(t)
	ex := h.as(sidAlice, false, sidAlice)

	for _, req := range []BrokerRequest{
		{Op: "powershell", Name: "dev"},
		{Op: ""},
		{Op: "Start", Name: "dev"},
		{Op: opStart, Name: "dev", Create: &BrokerCreate{}},
	} {
		if _, err := ex.call(context.Background(), req); err == nil || brokerCode(t, err) != codeInvalid {
			t.Errorf("%+v: %v", req, err)
		}
	}

	// A field the protocol does not have, such as a path, is refused rather
	// than ignored.
	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte(`{"op":"create","name":"dev","create":{"rootfs":"C:\\Windows\\x.vhdx"}}`))
	buf := make([]byte, 512)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), codeInvalid) || h.fake.got() != "" {
		t.Errorf("answer %q, calls %q", buf[:n], h.fake.got())
	}
}

func TestBrokerFailedCreateTakesItsPartMachineAway(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	h.fake.createErr = errors.New("New-VM failed")
	alice := h.as(sidAlice, false, sidAlice)

	if err := alice.Create(ctx, sampleSpec("dev")); err == nil || !strings.Contains(err.Error(), "New-VM failed") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(h.fake.got(), "destroy dev") {
		t.Errorf("calls = %s: the partial machine was left", h.fake.got())
	}
	// The name is free again.
	h.fake.createErr = nil
	if err := alice.Create(ctx, sampleSpec("dev")); err != nil {
		t.Errorf("retry: %v", err)
	}
}

func TestBrokerPurge(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	for _, c := range []struct {
		sid, name string
	}{{sidAlice, "a"}, {sidBob, "b"}} {
		if err := h.as(c.sid, false, c.sid).Create(ctx, sampleSpec(c.name)); err != nil {
			t.Fatal(err)
		}
	}
	h.fake.vms["unmanaged"] = Running

	_, err := h.as(sidAlice, false, sidAlice).call(ctx, BrokerRequest{Op: opPurge})
	if err == nil || brokerCode(t, err) != codeNotAllowed {
		t.Errorf("a member purged: %v", err)
	}

	resp, err := h.as(sidSystem, true).call(ctx, BrokerRequest{Op: opPurge})
	if err != nil || !slices.Equal(resp.Purged, []string{"a", "b"}) {
		t.Errorf("purged %v, %v", resp.Purged, err)
	}
	if _, ok := h.fake.vms["unmanaged"]; !ok || len(h.fake.vms) != 1 {
		t.Errorf("vms = %v: only managed machines go", h.fake.vms)
	}
}

// Every operation the backend performs reaches the direct executor through
// the broker executor, with the same bare name and the same answer.
func TestBrokerExecutorRoundTripsEveryOperation(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	b := hyperVBackend{exec: h.as(sidAlice, false, sidAlice), broker: true}

	if b.Name() != "hyperv-broker" {
		t.Errorf("Name = %q", b.Name())
	}
	if err := b.Available(ctx); err != nil {
		t.Fatal(err)
	}
	if obs, _ := b.Inspect(ctx, "dev"); obs.State != Absent {
		t.Errorf("inspect before create = %+v", obs)
	}
	if err := b.Create(ctx, sampleSpec("dev")); err != nil {
		t.Fatal(err)
	}
	obs, err := b.Inspect(ctx, "dev")
	if err != nil || obs.State != Running || obs.Generation != "g" {
		t.Errorf("inspect = %+v, %v", obs, err)
	}
	if err := b.Enrol(ctx, "dev", "alice", "k"); err != nil {
		t.Errorf("enrol: %v", err) // the notes' key fingerprint came through
	}
	if err := b.Enrol(ctx, "dev", "alice", "another key"); err == nil {
		t.Error("a different key was accepted: the notes did not cross")
	}
	_ = b.Start(ctx, "dev")
	_ = b.Stop(ctx, "dev")
	_ = b.Retract(ctx, "dev")
	if addr, err := b.Address(ctx, "dev"); err != nil || addr != "172.20.0.9" {
		t.Errorf("address = %q, %v", addr, err)
	}
	if err := b.Destroy(ctx, "dev"); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"available", "create dev", "start dev", "stop dev", "retract dev", "address dev", "destroy dev"} {
		if !strings.Contains(h.fake.got(), want) {
			t.Errorf("calls %q lack %q", h.fake.got(), want)
		}
	}
}

func TestBrokerExecutorRefusesARootfsOfItsOwn(t *testing.T) {
	h := newBrokerHarness(t)
	spec := sampleSpec("dev")
	spec.Fetched, spec.Rootfs = false, `C:\Users\alice\x.vhdx`
	err := h.as(sidAlice, false, sidAlice).Create(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "--rootfs") || h.fake.got() != "" {
		t.Errorf("err = %v, calls %q", err, h.fake.got())
	}
}

func TestBrokerNotAllowedNamesTheGroup(t *testing.T) {
	msg := (&BrokerError{Code: codeNotAllowed}).Error()
	if !strings.Contains(msg, "\n  fix: ask an administrator to add you to remote-docker-users (net localgroup remote-docker-users <you> /add)") {
		t.Errorf("message = %q", msg)
	}
}

func TestBrokerRecordsAreAtomicAndNeverOverwriteWhatTheyCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machines.json")
	r := brokerRecords{path: path}
	if err := r.put("dev", brokerRecord{Owner: sidAlice}); err != nil {
		t.Fatal(err)
	}
	m, err := r.load()
	if err != nil || m["dev"].Owner != sidAlice {
		t.Fatalf("load = %v, %v", m, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.put("other", brokerRecord{Owner: sidBob}); err == nil {
		t.Error("an unreadable record was overwritten")
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{not json" {
		t.Errorf("file = %q", raw)
	}
}

func TestBrokerConstantsArePinned(t *testing.T) {
	// A change to either descriptor changes who can reach SYSTEM's service, so
	// it is made on purpose.
	if brokerPipeSDDL != "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;AU)" {
		t.Errorf("pipe SDDL = %s", brokerPipeSDDL)
	}
	if brokerDirSDDL != "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)" {
		t.Errorf("directory SDDL = %s", brokerDirSDDL)
	}
	if BackendKey("hyperv", true) != "hyperv-broker" || BackendKey("hyperv", false) != "hyperv" || BackendKey("wsl", true) != "wsl" {
		t.Error("BackendKey")
	}
	if g1, g2 := (Spec{Name: "a"}).Generation(), (Spec{Name: "a", Broker: true}).Generation(); g1 != g2 {
		t.Error("the broker flag changed the generation: a machine cannot move between them")
	}
}

// waitCalls waits until the fake has seen n calls of op.
func waitCalls(t *testing.T, f *brokerFake, op string, n int) {
	t.Helper()
	for range 200 {
		f.mu.Lock()
		got := strings.Count(","+strings.Join(f.calls, ",")+",", ","+op+" ")
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never saw %d %s calls: %s", n, op, f.got())
}

// One caller cannot take every slot: a third operation from the same SID is
// refused while two hang, and neither another member nor an administrator is
// held up by it.
func TestBrokerCapsOperationsPerCaller(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	h.fake.block = make(chan struct{})
	defer close(h.fake.block)

	alice := h.as(sidAlice, false, sidAlice, sidBob)
	for i := range perCallerOps {
		go func() { _, _, _ = alice.Look(ctx, fmt.Sprintf("m%d", i)) }()
	}
	waitCalls(t, h.fake, "look", perCallerOps)

	if _, _, err := alice.Look(ctx, "m9"); err == nil || brokerCode(t, err) != codeBusy {
		t.Fatalf("a third operation: %v, want busy", err)
	}

	// Others get through to the fake (and then wait with it, so ask
	// something that does not look).
	if err := h.as(sidBob, false, sidAlice, sidBob).Available(ctx); err != nil {
		t.Errorf("another member was held up: %v", err)
	}
	if err := h.as(sidSystem, true).Available(ctx); err != nil {
		t.Errorf("an administrator was held up: %v", err)
	}
}

// A Hyper-V call that never returns ends at the operation's deadline, and the
// caller is told.
func TestBrokerOperationTimesOut(t *testing.T) {
	saved := brokerOpTimeout
	brokerOpTimeout = 150 * time.Millisecond
	defer func() { brokerOpTimeout = saved }()

	h := newBrokerHarness(t)
	h.fake.block = make(chan struct{}) // never released
	start := time.Now()
	_, _, err := h.as(sidAlice, false, sidAlice).Look(context.Background(), "dev")
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %v, want the deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

// The operation stops when the caller's connection goes away.
func TestBrokerOperationStopsWhenTheCallerDisconnects(t *testing.T) {
	h := newBrokerHarness(t)
	h.fake.block = make(chan struct{})
	h.fake.ctxDone = make(chan struct{}, 1)
	h.as(sidAlice, false, sidAlice)

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte(`{"op":"look","name":"dev"}`))
	waitCalls(t, h.fake, "look", 1)
	_ = conn.Close()

	select {
	case <-h.fake.ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the operation kept running after its caller left")
	}
}

// A create that fails and cannot remove what it made keeps the record, so the
// machine can still be destroyed, by its owner or by a purge.
func TestBrokerFailedCreateWithFailedCleanupKeepsTheRecord(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	h.fake.createErr = errors.New("New-VM failed")
	h.fake.destroyErr = errors.New("Remove-VM failed")
	alice := h.as(sidAlice, false, sidAlice)

	err := alice.Create(ctx, sampleSpec("dev"))
	if err == nil || !strings.Contains(err.Error(), "Remove-VM failed") || !strings.Contains(err.Error(), "remote rm dev") {
		t.Fatalf("err = %v", err)
	}
	recs, _ := h.srv.records.load()
	if recs["dev"].Owner != sidAlice {
		t.Fatalf("records = %v: the orphan is no longer anybody's", recs)
	}

	// Nobody else can take the name, and the owner can finish the job.
	if err := h.as(sidBob, false, sidBob).Create(ctx, sampleSpec("dev")); err == nil {
		t.Error("another member took the name of an orphan")
	}
	h.fake.destroyErr = nil
	alice = h.as(sidAlice, false, sidAlice)
	if err := alice.Destroy(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if recs, _ := h.srv.records.load(); len(recs) != 0 {
		t.Errorf("records = %v after destroy", recs)
	}
}

// A destroy that finishes after the record has changed hands does not delete
// the new owner's record.
func TestBrokerDestroyKeepsAnotherOwnersRecord(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	alice := h.as(sidAlice, false, sidAlice)
	if err := alice.Create(ctx, sampleSpec("dev")); err != nil {
		t.Fatal(err)
	}
	h.fake.onDestroy = func() {
		_ = h.srv.records.put("dev", brokerRecord{Owner: sidBob})
	}
	if err := alice.Destroy(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	if recs, _ := h.srv.records.load(); recs["dev"].Owner != sidBob {
		t.Errorf("records = %v: alice's destroy removed bob's record", recs)
	}
}

// No lock is held across a Hyper-V call: a hung operation on one machine does
// not stop a record from being read for another.
func TestBrokerOneHungMachineDoesNotBlockAnother(t *testing.T) {
	ctx := context.Background()
	h := newBrokerHarness(t)
	if err := h.as(sidAlice, false, sidAlice).Create(ctx, sampleSpec("fast")); err != nil {
		t.Fatal(err)
	}
	h.fake.block = make(chan struct{})
	defer close(h.fake.block)
	hung := h.as(sidAlice, false, sidAlice)
	go func() { _, _, _ = hung.Look(ctx, "hung") }()
	waitCalls(t, h.fake, "look", 2) // create's, then the hung one

	h.fake.block = nil
	done := make(chan error, 1)
	go func() { done <- h.as(sidBob, false, sidBob).Start(ctx, "fast") }()
	select {
	case err := <-done:
		if err == nil || brokerCode(t, err) != codeNotYours {
			t.Errorf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call on another machine waited on the hung one")
	}
}
