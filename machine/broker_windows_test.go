//go:build windows

package machine

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// self is this test process's own identity as the service would read it.
func self(t *testing.T) brokerCaller {
	t.Helper()
	tok := windows.GetCurrentProcessToken()
	c, err := callerOfToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A real named pipe, the real SID read off it and the real SDDL, with a fake
// Hyper-V behind it.
func TestBrokerOverARealNamedPipe(t *testing.T) {
	me := self(t)
	ctx := context.Background()

	path := fmt.Sprintf(`\\.\pipe\remote-docker-test-%d`, os.Getpid())
	l, err := listenPipe(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()

	fake := newBrokerFake()
	var (
		mu      sync.Mutex
		members []string
	)
	srv := newBrokerServer(fake, filepath.Join(t.TempDir(), "machines.json"), pipeCaller,
		func() ([]string, error) { mu.Lock(); defer mu.Unlock(); return members, nil },
		func(context.Context, string) (string, error) { return `C:\cache\flatcar.vhdx`, nil })
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = srv.Serve(sctx, l) }()

	// This process is the "service" here, so its pipe is owned by this user and
	// not by SYSTEM; the real check is exercised below.
	trusted := pipeServerTrusted
	pipeServerTrusted = func(*windows.SID) bool { return true }
	defer func() { pipeServerTrusted = trusted }()

	ex := brokerExecutor{dial: func(ctx context.Context) (net.Conn, error) { return dialPipe(ctx, path) }}

	if !me.Admin {
		// Not in the group: refused, and told so, over the real pipe.
		err := ex.Available(ctx)
		if err == nil || brokerCode(t, err) != codeNotAllowed {
			t.Fatalf("a non-member was not refused: %v", err)
		}
	}

	// The SID the service read off the pipe is this process's.
	mu.Lock()
	members = []string{me.SID}
	mu.Unlock()
	b := hyperVBackend{exec: ex, broker: true}
	if err := b.Create(ctx, sampleSpec("dev")); err != nil {
		t.Fatal(err)
	}
	if obs, err := b.Inspect(ctx, "dev"); err != nil || obs.State != Running {
		t.Errorf("inspect = %+v, %v", obs, err)
	}
	recs, err := srv.records.load()
	if err != nil || !strings.EqualFold(recs["dev"].Owner, me.SID) {
		t.Errorf("recorded owner = %q, want %q (%v)", recs["dev"].Owner, me.SID, err)
	}
	if err := b.Destroy(ctx, "dev"); err != nil {
		t.Error(err)
	}
}

// A pipe created by an ordinary user is not the service's, and the client
// refuses to send it anything.
func TestBrokerClientRefusesAPipeNotOwnedBySystem(t *testing.T) {
	if self(t).Admin {
		t.Skip("an elevated token's pipes are owned by Administrators, which is trusted")
	}
	path := fmt.Sprintf(`\\.\pipe\remote-docker-squat-%d`, os.Getpid())
	l, err := listenPipe(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		if c, err := l.Accept(); err == nil {
			_ = c.Close()
		}
	}()

	_, err = dialPipe(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "refusing to send it anything") {
		t.Errorf("err = %v", err)
	}
}

func TestLocalGroupMembersReadsTheRealGroup(t *testing.T) {
	// By SID, because the group's name is localised.
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	name, _, _, err := admins.LookupAccount("")
	if err != nil {
		t.Fatal(err)
	}
	sids, exists, err := localGroupMembers(name)
	if err != nil || !exists || len(sids) == 0 {
		t.Fatalf("Administrators = %v, %v, %v", sids, exists, err)
	}
	for _, s := range sids {
		if !strings.HasPrefix(s, "S-1-") {
			t.Errorf("%q is not a SID", s)
		}
	}
	if sids, exists, err := localGroupMembers("remote-docker-test-no-such-group"); exists || len(sids) != 0 || err != nil {
		t.Errorf("a missing group = %v, %v, %v", sids, exists, err)
	}
}

// The service's directory is born protected: SYSTEM and Administrators only,
// nothing inherited from a parent where Users may create things.
func TestProtectedDirectoryHasOnlySystemAndAdministrators(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote-docker")
	if err := ensureProtectedDir(dir); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	got := sd.String()
	if !strings.HasPrefix(got, "D:P") {
		t.Errorf("DACL %s is not protected", got)
	}
	aces := strings.Count(got, "(")
	if aces != 2 || !strings.Contains(got, ";;;SY)") || !strings.Contains(got, ";;;BA)") {
		t.Errorf("DACL = %s, want exactly SYSTEM and Administrators", got)
	}
}

func TestDirectoryThatIsAReparsePointIsRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	// A junction needs no privilege, unlike a symlink.
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v %s", err, out)
	}
	if err := ensureProtectedDir(link); err == nil {
		t.Error("a symlinked directory was adopted")
	}
}

// Setup and teardown against the real local groups. Runs only elevated, as CI
// does, and only on a computer that has none of this installed.
func TestBrokerSetupAndTeardownAreIdempotent(t *testing.T) {
	me := self(t)
	if !me.Admin {
		t.Skip("creating a local group needs an elevated token")
	}
	root, err := brokerRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, exists, err := localGroupMembers(BrokerGroup)
	if _, serr := os.Stat(root); exists || serr == nil || err != nil {
		t.Skip("the service is installed here")
	}
	ctx := context.Background()
	t.Cleanup(func() { _ = BrokerTeardown(ctx) })

	for range 2 {
		if err := BrokerSetup(me.SID); err != nil {
			t.Fatal(err)
		}
	}
	members, exists, err := localGroupMembers(BrokerGroup)
	if err != nil || !exists || len(members) != 1 || !strings.EqualFold(members[0], me.SID) {
		t.Fatalf("members = %v, %v, %v", members, exists, err)
	}

	for range 2 {
		if err := BrokerTeardown(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists, _ := localGroupMembers(BrokerGroup); exists {
		t.Error("the group it created is still there")
	}
}
