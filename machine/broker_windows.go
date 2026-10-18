//go:build windows

package machine

// The Windows half of the broker: the pipe, who is on it, the live group, the
// service control manager and the service's directory (ADR 0055).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func init() {
	registered = append(registered, hyperVBackend{exec: brokerExecutor{dial: dialBroker}, broker: true})
}

// x/sys/windows wraps none of these.
var (
	advapi32                     = windows.NewLazySystemDLL("advapi32.dll")
	procImpersonateNamedPipeClnt = advapi32.NewProc("ImpersonateNamedPipeClient")
	netapi32                     = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupGetMembers  = netapi32.NewProc("NetLocalGroupGetMembers")
	procNetLocalGroupAdd         = netapi32.NewProc("NetLocalGroupAdd")
	procNetLocalGroupDel         = netapi32.NewProc("NetLocalGroupDel")
	procNetLocalGroupAddMembers  = netapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupDelMembers  = netapi32.NewProc("NetLocalGroupDelMembers")
)

type syscallErrno = windows.Errno

// brokerPipe is where the service listens.
const brokerPipe = `\\.\pipe\remote-docker-machines`

// brokerRoot is %ProgramData%\remote-docker, asked of the system and not of
// the environment: this runs as SYSTEM, and a variable is something a caller
// of the installer's custom action could have set.
func brokerRoot() (string, error) {
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(pd, "remote-docker"), nil
}

// ---- who is on the pipe ---------------------------------------------------

// pipeCaller identifies the process on the other end of a pipe connection from
// the operating system alone: it impersonates the client on a thread of its
// own, reads the token's user and whether Administrators is enabled in it, and
// reverts. Nothing is read from the pipe.
func pipeCaller(conn net.Conn) (brokerCaller, error) {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return brokerCaller{}, errors.New("not a named pipe connection")
	}
	type result struct {
		c   brokerCaller
		err error
	}
	ch := make(chan result, 1)
	go func() {
		// The thread is impersonating between the two calls. If reverting
		// fails it is never unlocked, and a locked thread whose goroutine
		// exits is terminated by the runtime rather than reused.
		runtime.LockOSThread()
		c, err := impersonatedCaller(windows.Handle(f.Fd()))
		if rerr := windows.RevertToSelf(); rerr != nil {
			ch <- result{err: fmt.Errorf("reverting impersonation: %w", rerr)}
			return
		}
		runtime.UnlockOSThread()
		ch <- result{c, err}
	}()
	r := <-ch
	return r.c, r.err
}

func impersonatedCaller(pipe windows.Handle) (brokerCaller, error) {
	if r, _, err := procImpersonateNamedPipeClnt.Call(uintptr(pipe)); r == 0 {
		return brokerCaller{}, fmt.Errorf("impersonating the caller: %w", err)
	}
	thread := windows.CurrentThread()
	var tok windows.Token
	// openAsSelf: the thread's token is opened with the service's rights, not
	// the caller's, which could not open its own.
	if err := windows.OpenThreadToken(thread, windows.TOKEN_QUERY, true, &tok); err != nil {
		return brokerCaller{}, fmt.Errorf("reading the caller's token: %w", err)
	}
	defer func() { _ = tok.Close() }()
	return callerOfToken(tok)
}

// callerOfToken reads the user and whether Administrators is enabled. The
// groups are read from the token and not checked with CheckTokenMembership,
// which wants an impersonation level a client does not have to grant. A
// UAC-filtered administrator's group is deny-only and so does not count: that
// is an administrator who has not elevated, and the group decides for them.
func callerOfToken(tok windows.Token) (brokerCaller, error) {
	user, err := tok.GetTokenUser()
	if err != nil {
		return brokerCaller{}, err
	}
	c := brokerCaller{SID: user.User.Sid.String(), Admin: user.User.Sid.IsWellKnown(windows.WinLocalSystemSid)}
	if c.Admin {
		return c, nil
	}
	groups, err := tok.GetTokenGroups()
	if err != nil {
		return brokerCaller{}, err
	}
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_ENABLED != 0 && g.Sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			c.Admin = true
		}
	}
	return c, nil
}

// ---- the live group -------------------------------------------------------

const (
	errGroupMissing   syscallErrno = 1376 // ERROR_NO_SUCH_ALIAS
	errMemberNoAlias  syscallErrno = 1377
	errMemberInAlias  syscallErrno = 1378
	errAliasExists    syscallErrno = 1379
	nerrGroupNotFound syscallErrno = 2220
	nerrGroupExists   syscallErrno = 2223
)

// localGroupMembers lists the SIDs that are DIRECT members of a local group,
// now. A group that does not exist has none. Level 0 of netapi32, which is
// SIDs and not names, so a rename or a localised name changes nothing.
func localGroupMembers(group string) (sids []string, exists bool, err error) {
	name, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return nil, false, err
	}
	var (
		buf         *byte
		read, total uint32
		resume      uintptr
	)
	r, _, _ := procNetLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(name)), 0,
		uintptr(unsafe.Pointer(&buf)), 0xFFFFFFFF,
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resume)))
	if buf != nil {
		defer func() { _ = windows.NetApiBufferFree(buf) }()
	}
	switch syscallErrno(r) {
	case 0:
	case errGroupMissing, nerrGroupNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("NetLocalGroupGetMembers(%s): %w", group, syscallErrno(r))
	}
	// LOCALGROUP_MEMBERS_INFO_0 is one PSID.
	for _, sid := range unsafe.Slice((**windows.SID)(unsafe.Pointer(buf)), read) {
		sids = append(sids, sid.String())
	}
	return sids, true, nil
}

func brokerGroupMembers() ([]string, error) {
	sids, _, err := localGroupMembers(BrokerGroup)
	return sids, err
}

// netCall runs one netapi32 call, treating the codes in ok as success.
func netCall(proc *windows.LazyProc, ok []syscallErrno, args ...uintptr) error {
	r, _, _ := proc.Call(args...)
	e := syscallErrno(r)
	if e == 0 {
		return nil
	}
	for _, o := range ok {
		if e == o {
			return nil
		}
	}
	return e
}

func addLocalGroup(group string) error {
	name, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	var parm uint32
	return netCall(procNetLocalGroupAdd, []syscallErrno{errAliasExists, nerrGroupExists},
		0, 0, uintptr(unsafe.Pointer(&name)), uintptr(unsafe.Pointer(&parm)))
}

func deleteLocalGroup(group string) error {
	name, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	return netCall(procNetLocalGroupDel, []syscallErrno{errGroupMissing, nerrGroupNotFound}, 0, uintptr(unsafe.Pointer(name)))
}

func changeLocalGroupMember(proc *windows.LazyProc, group string, sid *windows.SID, ok ...syscallErrno) error {
	name, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	return netCall(proc, ok, 0, uintptr(unsafe.Pointer(name)), 0, uintptr(unsafe.Pointer(&sid)), 1)
}

// ---- the service's directory ---------------------------------------------

// protectedSecurity returns attributes carrying the protected DACL, so a
// directory is born with it and there is no moment it inherits from a parent
// where Users may create things.
func protectedSecurity(owner string) (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	sd, err := windows.SecurityDescriptorFromString(owner + brokerDirSDDL)
	if err != nil {
		return nil, nil, err
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}, sd, nil
}

// ensureProtectedDir makes a directory only SYSTEM and Administrators can
// use. One that already exists is accepted only if SYSTEM or Administrators
// own it and it is not a reparse point: ProgramData lets any user create a
// directory, so a name pre-created there is a trap and the service refuses to
// start on it rather than adopt it.
func ensureProtectedDir(path string) error {
	// Owned by Administrators, whoever creates it: a directory owned by the
	// installing user would be refused by verifyProtectedDir. Only SYSTEM or an
	// elevated administrator may assign that owner; anyone else gets a
	// directory under their own, which nothing here will ever accept.
	sa, sd, err := protectedSecurity("O:BA")
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	err = windows.CreateDirectory(p, sa)
	if errors.Is(err, windows.ERROR_INVALID_OWNER) {
		sa, sd, err = protectedSecurity("")
		if err != nil {
			return err
		}
		err = windows.CreateDirectory(p, sa)
	}
	if err == nil {
		return nil
	}
	if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	if err := verifyProtectedDir(path); err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// verifyProtectedDir refuses a directory that is not a plain directory owned
// by SYSTEM or Administrators. Everything that reads or deletes under the
// service's root asks this first: ProgramData lets any user create a folder
// there, and a user's folder or junction must never be what SYSTEM reads
// machines.json from or removes.
func verifyProtectedDir(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("%s is not a plain directory", path)
	}
	info, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := info.Owner()
	if err != nil {
		return err
	}
	if !owner.IsWellKnown(windows.WinLocalSystemSid) && !owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
		return fmt.Errorf("%s is owned by %s, not by SYSTEM or Administrators", path, owner)
	}
	return nil
}

// ensureBrokerDirs creates the root and the two directories under it.
func ensureBrokerDirs(root string) error {
	for _, d := range []string{root, filepath.Join(root, "machines"), filepath.Join(root, "rootfs")} {
		if err := ensureProtectedDir(d); err != nil {
			return err
		}
	}
	return nil
}

// ---- the pipe -------------------------------------------------------------

func listenBroker() (net.Listener, error) { return listenPipe(brokerPipe, brokerPipeSDDL) }

// listenPipe takes the descriptor so a test, whose server is an ordinary user
// and not SYSTEM, can add itself: every instance after the first is created by
// the listener and needs the right that Authenticated Users deliberately lack.
func listenPipe(path, sddl string) (net.Listener, error) {
	return winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
}

// dialBroker connects to the service, starting it first when it is stopped.
// Identification level only: the service reads who we are and does not act as
// us.
func dialBroker(ctx context.Context) (net.Conn, error) {
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := startBrokerService(startCtx); err != nil {
		return nil, err
	}
	return dialPipe(ctx, brokerPipe)
}

// pipeClientAccess is what a client opens the pipe for: the rights the pipe's
// DACL gives Authenticated Users and no more. GENERIC_WRITE would ask for
// FILE_APPEND_DATA, which on a pipe is creating an instance, and be refused.
const pipeClientAccess = 0x12019b

// dialPipe connects to the pipe, waiting for it to exist.
func dialPipe(ctx context.Context, path string) (net.Conn, error) {
	// The dial itself waits for a busy pipe until the context ends.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := winio.DialPipeAccessImpLevel(ctx, path,
			pipeClientAccess, winio.PipeImpLevelIdentification)
		if err == nil {
			if verr := verifyPipeServer(conn); verr != nil {
				_ = conn.Close()
				return nil, verr
			}
			return conn, nil
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			return nil, fmt.Errorf("the Hyper-V service is not answering on %s: %w", brokerPipe, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// verifyPipeServer refuses a pipe the service does not serve. The process on
// the other end must be the service's own process: a user who creates the pipe
// while the service is stopped, or a second instance of it while it runs
// (the pipe's owner belongs to the name and says nothing about which instance
// answers), would otherwise be sent the request, private host key included.
func verifyPipeServer(conn net.Conn) error {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("not a named pipe connection")
	}
	refuse := func(why string) error {
		return fmt.Errorf("%s is not served by the Hyper-V service (%s); refusing to send it anything", brokerPipe, why)
	}
	var pipePID uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pipePID); err != nil {
		return refuse(err.Error())
	}
	svcPID, err := servicePID()
	if err != nil {
		return refuse(err.Error())
	}
	if pipePID == 0 || pipePID != svcPID {
		return refuse(fmt.Sprintf("process %d answers, the service is process %d", pipePID, svcPID))
	}
	return nil
}

// servicePID is the process id of the running service. A variable for the
// tests, whose server is the test process.
var servicePID = brokerServicePID

func brokerServicePID() (uint32, error) {
	svc, err := openBrokerService(windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return 0, fmt.Errorf("the service is not available: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(svc) }()
	var st windows.SERVICE_STATUS_PROCESS
	var needed uint32
	if err := windows.QueryServiceStatusEx(svc, windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&st)), uint32(unsafe.Sizeof(st)), &needed); err != nil {
		return 0, err
	}
	if st.CurrentState != windows.SERVICE_RUNNING || st.ProcessId == 0 {
		return 0, errors.New("the service is not running")
	}
	return st.ProcessId, nil
}

// ---- the service control manager -----------------------------------------

const scmAccessConnect = 0x0001

// BrokerInstalled reports whether the Hyper-V service is installed. A standard
// user can ask: connecting to the manager and querying a service need no
// rights.
func BrokerInstalled() (bool, error) {
	svc, err := openBrokerService(windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = windows.CloseServiceHandle(svc)
	return true, nil
}

func openBrokerService(access uint32) (windows.Handle, error) {
	scm, err := windows.OpenSCManager(nil, nil, scmAccessConnect)
	if err != nil {
		return 0, fmt.Errorf("connecting to the service manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	name, err := windows.UTF16PtrFromString(BrokerServiceName)
	if err != nil {
		return 0, err
	}
	return windows.OpenService(scm, name, access)
}

// startBrokerService starts the service if it is stopped and waits for it to
// run. The installer grants Authenticated Users the right to start it and
// nothing else; who may DO anything is decided inside. StartService is called
// once: a service that stops again straight away is reported, not restarted
// every few hundred milliseconds by whoever asked.
func startBrokerService(ctx context.Context) error {
	svc, err := openBrokerService(windows.SERVICE_QUERY_STATUS | windows.SERVICE_START)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return ErrBrokerNotInstalled
		}
		return fmt.Errorf("opening the Hyper-V service: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(svc) }()

	started := false
	for {
		var st windows.SERVICE_STATUS
		if err := windows.QueryServiceStatus(svc, &st); err != nil {
			return err
		}
		switch st.CurrentState {
		case windows.SERVICE_RUNNING:
			return nil
		case windows.SERVICE_STOPPED:
			if started {
				return errors.New("the Hyper-V service stopped right after starting; see the Application event log")
			}
			started = true
			err := windows.StartService(svc, 0, nil)
			if err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return fmt.Errorf("starting the Hyper-V service: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// brokerServiceRunning reports whether the service process is up.
func brokerServiceRunning() (bool, error) {
	svc, err := openBrokerService(windows.SERVICE_QUERY_STATUS)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseServiceHandle(svc) }()
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(svc, &st); err != nil {
		return false, err
	}
	return st.CurrentState != windows.SERVICE_STOPPED, nil
}
