package machine

// The service's logic: who may ask, whose a machine is, and what each typed
// operation does. Everything platform-specific (the pipe, the SID, the group)
// is injected, so all of it is tested on any platform (ADR 0055).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// brokerCaller is who is on the other end of a connection. Both fields come
// from the operating system's view of the pipe, never from a request.
type brokerCaller struct {
	SID string
	// Admin is SYSTEM, or a token with Administrators enabled.
	Admin bool
}

// maxBrokerRequest bounds what is read from a caller, after it is authorised.
const maxBrokerRequest = 64 << 10

// Limits. Variables so the tests can shrink them.
var (
	// brokerOpTimeout bounds every operation but create and purge. A Hyper-V
	// call that hangs holds its machine's name, and a caller's slot, until this.
	brokerOpTimeout = 2 * time.Minute
	// brokerCreateTimeout covers the Flatcar download (flatcarDownloadTimeout)
	// and building the machine.
	brokerCreateTimeout = 45 * time.Minute
	brokerPurgeTimeout  = 15 * time.Minute

	// maxBrokerConns bounds connections being served at all, authorised or not.
	maxBrokerConns = 32
	// perCallerOps is how many operations one SID may have running, so one
	// member cannot take every slot. Administrators count against their own,
	// separate limit, so purge and an administrator are never locked out by
	// members.
	perCallerOps = 2
	perAdminOps  = 4
)

type brokerServer struct {
	exec    hyperVExecutor
	records brokerRecords

	// identify says who is on the pipe. Called before anything is read, and
	// reads nothing, so an idle connection holds no more than a goroutine.
	identify func(net.Conn) (brokerCaller, error)

	// members lists the SIDs in BrokerGroup, now. Asked on every connection
	// and never cached: a user added a second ago is allowed without signing
	// out, which the token's group list could not do.
	members func() ([]string, error)

	// fetchDisk returns the Flatcar disk, downloading it into the service's
	// own cache.
	fetchDisk func(ctx context.Context, proxy string) (string, error)

	now func() time.Time

	// mu guards machines.json and nothing else: it is held for a file read or
	// write and never across a Hyper-V or PowerShell call.
	mu sync.Mutex
	// names serialises everything done to one machine, from the access check to
	// the record update, so the owner read at the start is still the owner at
	// the end.
	names nameLocks

	conns    chan struct{}
	capMu    sync.Mutex
	inflight map[string]int
}

func newBrokerServer(exec hyperVExecutor, recordsPath string, identify func(net.Conn) (brokerCaller, error),
	members func() ([]string, error), fetchDisk func(context.Context, string) (string, error)) *brokerServer {
	return &brokerServer{
		exec: exec, records: brokerRecords{path: recordsPath},
		identify: identify, members: members, fetchDisk: fetchDisk,
		now: time.Now, conns: make(chan struct{}, maxBrokerConns), inflight: map[string]int{},
	}
}

// nameLocks is one lock per machine name that can be waited for with a
// context, so a wedged operation holds up its own machine only, and only until
// the waiter's deadline.
type nameLocks struct {
	mu sync.Mutex
	m  map[string]*nameLock
}

type nameLock struct {
	ch   chan struct{}
	refs int
}

func (n *nameLocks) lock(ctx context.Context, name string) (unlock func(), err error) {
	n.mu.Lock()
	if n.m == nil {
		n.m = map[string]*nameLock{}
	}
	l := n.m[name]
	if l == nil {
		l = &nameLock{ch: make(chan struct{}, 1)}
		n.m[name] = l
	}
	l.refs++
	n.mu.Unlock()

	release := func() {
		n.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(n.m, name)
		}
		n.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; release() }, nil
	case <-ctx.Done():
		release()
		return nil, fmt.Errorf("another operation on %q did not finish in time: %w", name, ctx.Err())
	}
}

// Serve answers connections until the listener closes.
func (s *brokerServer) Serve(ctx context.Context, l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case s.conns <- struct{}{}:
			go func() {
				defer func() { <-s.conns }()
				s.serveConn(ctx, conn)
			}()
		default:
			go func() {
				_ = conn.SetWriteDeadline(s.now().Add(2 * time.Second))
				_ = json.NewEncoder(conn).Encode(refusal(codeBusy, "the Hyper-V service has too many connections; try again"))
				_ = conn.Close()
			}()
		}
	}
}

// authorise decides whether a caller may use the service at all. Fails closed:
// a group that cannot be read allows nobody but an administrator.
func (s *brokerServer) authorise(c brokerCaller) error {
	if c.Admin {
		return nil
	}
	members, err := s.members()
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", BrokerGroup, err)
	}
	if slices.ContainsFunc(members, func(m string) bool { return strings.EqualFold(m, c.SID) }) {
		return nil
	}
	return errors.New("not a member of " + BrokerGroup)
}

// acquire takes one of the caller's operation slots.
func (s *brokerServer) acquire(c brokerCaller) (release func(), ok bool) {
	key, limit := c.SID, perCallerOps
	if c.Admin {
		key, limit = "admin:"+c.SID, perAdminOps
	}
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if s.inflight[key] >= limit {
		return nil, false
	}
	s.inflight[key]++
	return func() {
		s.capMu.Lock()
		defer s.capMu.Unlock()
		if s.inflight[key]--; s.inflight[key] <= 0 {
			delete(s.inflight, key)
		}
	}, true
}

func opTimeout(op string) time.Duration {
	switch op {
	case opCreate:
		return brokerCreateTimeout
	case opPurge:
		return brokerPurgeTimeout
	}
	return brokerOpTimeout
}

// serveConn is one request on one connection. The order is the point: identify,
// authorise, and only then read a byte. An unauthorised caller is told no and
// its input is never parsed, so a malformed request costs it nothing and us
// nothing.
func (s *brokerServer) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	reply := func(r BrokerResponse) {
		_ = conn.SetWriteDeadline(s.now().Add(10 * time.Second))
		_ = json.NewEncoder(conn).Encode(r)
	}

	caller, err := s.identify(conn)
	if err != nil {
		reply(refusal(codeNotAllowed, "cannot tell who is calling: "+err.Error()))
		return
	}
	if err := s.authorise(caller); err != nil {
		reply(refusal(codeNotAllowed, err.Error()))
		return
	}
	release, ok := s.acquire(caller)
	if !ok {
		reply(refusal(codeBusy, "you already have operations running in the Hyper-V service; wait for one to finish"))
		return
	}
	defer release()

	_ = conn.SetReadDeadline(s.now().Add(30 * time.Second))
	var req BrokerRequest
	dec := json.NewDecoder(io.LimitReader(conn, maxBrokerRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		reply(refusal(codeInvalid, "malformed request: "+err.Error()))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	// The operation ends at its deadline, or when the caller goes away: the
	// client sends nothing after its request, so a read that returns means the
	// connection dropped.
	opCtx, cancel := context.WithTimeout(ctx, opTimeout(req.Op))
	defer cancel()
	go func() {
		var b [1]byte
		if _, err := conn.Read(b[:]); err != nil {
			cancel()
		}
	}()

	reply(s.handle(opCtx, caller, req))
}

func refusal(code, msg string) BrokerResponse { return BrokerResponse{Code: code, Error: msg} }

func failure(err error) BrokerResponse { return refusal(codeFailed, err.Error()) }

var errAbsentUnowned = errors.New("no such machine")

func (s *brokerServer) loadRecords() (map[string]brokerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records.load()
}

func (s *brokerServer) putRecord(name string, rec brokerRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records.put(name, rec)
}

func (s *brokerServer) removeRecord(name, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records.removeIf(name, owner)
}

// access decides whether the caller may act on a machine that is not being
// created: it must be recorded for the caller's SID. A machine nobody recorded
// that does not exist either is errAbsentUnowned, which look reports as absent
// and destroy accepts; one that exists in Hyper-V and is nobody's the service
// knows is refused, and so is the name, whoever's the machine is. Called with
// the name's lock held.
func (s *brokerServer) access(ctx context.Context, name string, c brokerCaller) error {
	recs, err := s.loadRecords()
	if err != nil {
		return err
	}
	if rec, ok := recs[name]; ok {
		if strings.EqualFold(rec.Owner, c.SID) {
			return nil
		}
		return &BrokerError{Code: codeNotYours, Message: "the workspace name is taken by another user of this computer\n  fix: choose another workspace name"}
	}
	state, _, err := s.exec.Look(ctx, name)
	if err != nil {
		return err
	}
	if state == Absent {
		return errAbsentUnowned
	}
	return &BrokerError{Code: codeNotYours, Message: "the workspace name is taken by a machine this service does not manage\n  fix: choose another workspace name"}
}

func (s *brokerServer) handle(ctx context.Context, c brokerCaller, req BrokerRequest) BrokerResponse {
	switch req.Op {
	case opAvailable:
		if err := s.exec.Available(ctx); err != nil {
			return failure(err)
		}
		return BrokerResponse{OK: true}
	case opPurge:
		return s.purge(ctx, c)
	case opCreate:
		return s.create(ctx, c, req)
	case opLook, opStart, opStop, opAddress, opRetract, opDestroy:
	default:
		return refusal(codeInvalid, fmt.Sprintf("unknown operation %q", req.Op))
	}

	if err := validBrokerName(req.Name); err != nil {
		return refusal(codeInvalid, err.Error())
	}
	if req.Create != nil {
		return refusal(codeInvalid, "only create takes create settings")
	}

	unlock, err := s.names.lock(ctx, req.Name)
	if err != nil {
		return failure(err)
	}
	defer unlock()

	err = s.access(ctx, req.Name, c)
	absent := errors.Is(err, errAbsentUnowned)
	var be *BrokerError
	switch {
	case errors.As(err, &be):
		return refusal(be.Code, be.Message)
	case err != nil && !absent:
		return failure(err)
	}

	switch req.Op {
	case opLook:
		if absent {
			return BrokerResponse{OK: true, State: stateName(Absent)}
		}
		state, notes, err := s.exec.Look(ctx, req.Name)
		if err != nil {
			return failure(err)
		}
		return BrokerResponse{OK: true, State: stateName(state), Notes: &BrokerNotes{Generation: notes.Generation, Key: notes.Key}}
	case opDestroy:
		if absent {
			return BrokerResponse{OK: true}
		}
		if err := s.exec.Destroy(ctx, req.Name); err != nil {
			return failure(err)
		}
		// Only while the record is still this caller's.
		if err := s.removeRecord(req.Name, c.SID); err != nil {
			return failure(err)
		}
		return BrokerResponse{OK: true}
	}

	if absent {
		return refusal(codeFailed, "no such machine "+req.Name)
	}
	switch req.Op {
	case opStart:
		err = s.exec.Start(ctx, req.Name)
	case opStop:
		err = s.exec.Stop(ctx, req.Name)
	case opRetract:
		err = s.exec.Retract(ctx, req.Name)
	case opAddress:
		var addr string
		if addr, err = s.exec.Address(ctx, req.Name); err == nil {
			return BrokerResponse{OK: true, Address: addr}
		}
	}
	if err != nil {
		return failure(err)
	}
	return BrokerResponse{OK: true}
}

// create refuses a name that exists in the record or in Hyper-V, whoever owns
// it, so one user can never be handed another's machine. The record is written
// BEFORE anything is built, so a service that dies halfway leaves something
// destroy can clean up. A failed create takes its own partial machine away,
// and keeps the record when that fails, so the machine stays destroyable.
func (s *brokerServer) create(ctx context.Context, c brokerCaller, req BrokerRequest) BrokerResponse {
	if err := validBrokerName(req.Name); err != nil {
		return refusal(codeInvalid, err.Error())
	}
	if req.Create == nil {
		return refusal(codeInvalid, "create needs settings")
	}
	if err := req.Create.validate(); err != nil {
		return refusal(codeInvalid, err.Error())
	}

	unlock, err := s.names.lock(ctx, req.Name)
	if err != nil {
		return failure(err)
	}
	defer unlock()

	recs, err := s.loadRecords()
	if err != nil {
		return failure(err)
	}
	if rec, ok := recs[req.Name]; ok {
		msg := "the workspace name is taken by another user of this computer\n  fix: choose another workspace name"
		if strings.EqualFold(rec.Owner, c.SID) {
			msg = "the workspace name is already recorded for you\n  fix: `remote rm " + req.Name + "` clears it"
		}
		return refusal(codeTaken, msg)
	}
	state, _, err := s.exec.Look(ctx, req.Name)
	if err != nil {
		return failure(err)
	}
	if state != Absent {
		return refusal(codeTaken, "the workspace name is taken by a machine this service does not manage\n  fix: choose another workspace name")
	}
	if err := s.putRecord(req.Name, brokerRecord{Owner: c.SID, Created: s.now().UTC().Format(time.RFC3339)}); err != nil {
		return failure(err)
	}

	buildErr := s.build(ctx, req)
	if buildErr == nil {
		return BrokerResponse{OK: true}
	}
	// What this call made, and nothing else: the name was Absent under this
	// lock and the record is ours. Not on the caller's context, which is the
	// reason it may be gone.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), brokerOpTimeout)
	defer cancel()
	if derr := s.exec.Destroy(cctx, req.Name); derr != nil {
		return failure(fmt.Errorf("%w; removing what it made also failed (%v), so the name stays recorded for you\n  fix: `remote rm %s` retries", buildErr, derr, req.Name))
	}
	if err := s.removeRecord(req.Name, c.SID); err != nil {
		return failure(errors.Join(buildErr, err))
	}
	return failure(buildErr)
}

func (s *brokerServer) build(ctx context.Context, req BrokerRequest) error {
	disk, err := s.fetchDisk(ctx, req.Create.Proxy)
	if err != nil {
		return err
	}
	return s.exec.Create(ctx, req.Create.spec(req.Name, disk))
}

// purge destroys every machine the service owns, for uninstalling. A machine
// that will not go stays recorded, so a retry finds it.
func (s *brokerServer) purge(ctx context.Context, c brokerCaller) BrokerResponse {
	if !c.Admin {
		return refusal(codeNotAllowed, "purge needs an administrator")
	}
	names, errs := s.purgeAll(ctx)
	if err := errors.Join(errs...); err != nil {
		return BrokerResponse{Code: codeFailed, Error: err.Error(), Purged: names}
	}
	return BrokerResponse{OK: true, Purged: names}
}

// purgeAll is the purge itself, also what teardown runs in its own process.
// One machine at a time under its name's lock, with no lock held across the
// Hyper-V call that is not that name's.
func (s *brokerServer) purgeAll(ctx context.Context) (done []string, errs []error) {
	recs, err := s.loadRecords()
	if err != nil {
		return nil, []error{err}
	}
	for _, name := range slices.Sorted(maps.Keys(recs)) {
		unlock, err := s.names.lock(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.exec.Destroy(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		} else if err := s.removeRecord(name, recs[name].Owner); err != nil {
			errs = append(errs, err)
		} else {
			done = append(done, name)
		}
		unlock()
	}
	return done, errs
}
