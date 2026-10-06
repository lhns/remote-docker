package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lhns/remote-docker/client/internal/session"
)

// A pending redeem (ADR 0051) waits for an account whose logins are refused
// until it exists.

func pendingInvite(t *testing.T, m *manageServer) string {
	t.Helper()
	withConfig(t, nil)
	t.Setenv("REMOTE_DOCKER_STATE_DIR", t.TempDir())
	return redeemInvite(t, m)
}

func shortenAccountWait(t *testing.T, poll, limit time.Duration) {
	t.Helper()
	p, l := session.AccountPollInterval, session.AccountWaitLimit
	session.AccountPollInterval, session.AccountWaitLimit = poll, limit
	t.Cleanup(func() { session.AccountPollInterval, session.AccountWaitLimit = p, l })
}

func TestAPendingRedeemWaitsUntilTheAccountLogsIn(t *testing.T) {
	shortenAccountWait(t, 10*time.Millisecond, 30*time.Second)
	m := startManageServer(t, `{"account":"alice","created":true,"pending":true}`, 0)
	m.refuse = 3
	out, err := runOut(t, "remote", "create", "ws", "--no-context", "--token", pendingInvite(t, m))
	if err != nil {
		t.Fatalf("create --token: %v\n%s", err, out)
	}
	if n := m.logins.Load(); n != 4 {
		t.Errorf("%d login attempts, want 3 refused and 1 accepted", n)
	}
	for _, want := range []string{"creating account alice on the workspace", "account alice is ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestAPendingRedeemThatNeverFinishesSaysSoAndKeepsTheConfig(t *testing.T) {
	shortenAccountWait(t, 10*time.Millisecond, 300*time.Millisecond)
	m := startManageServer(t, `{"account":"alice","created":true,"pending":true}`, 0)
	m.refuse = -1
	out, err := runOut(t, "remote", "create", "ws", "--token", pendingInvite(t, m))
	if err == nil || !strings.Contains(err.Error(), "has not finished creating account alice") {
		t.Fatalf("create --token: %v\n%s", err, out)
	}
	requireFixLine(t, err, "ask its operator to check the agent log")
	if strings.Contains(err.Error(), "token") {
		t.Errorf("the message suggests the token: %v", err)
	}
	if ws := savedWorkspace(t, "ws"); ws.User != "alice" {
		t.Errorf("saved %+v", ws)
	}
	if strings.Contains(out, "docker context") {
		t.Errorf("a context was created:\n%s", out)
	}
}

// The token is spent and the config saved by then, so a failure other than
// the account not being ready points at status, never at the token again.
func TestAPendingRedeemWhoseLoginFailsSaysItIsSaved(t *testing.T) {
	shortenAccountWait(t, 10*time.Millisecond, 30*time.Second)
	m := startManageServer(t, `{"account":"alice","created":true,"pending":true}`, 0)
	m.info = "WORKSPACE_NFS_PORT=20001\n" // no user: a workspace-info that cannot be read
	err := run(t, "remote", "create", "ws", "--no-context", "--token", pendingInvite(t, m))
	if err == nil || !strings.HasPrefix(err.Error(), `workspace "ws" is saved and the token spent, but the first login as alice failed: `) {
		t.Fatalf("create --token: %v", err)
	}
	requireFixLine(t, err, "remote status")
	if ws := savedWorkspace(t, "ws"); ws.User != "alice" {
		t.Errorf("saved %+v", ws)
	}
}

func TestAPendingRedeemStopsWaitingWhenCancelled(t *testing.T) {
	shortenAccountWait(t, 10*time.Millisecond, time.Minute)
	m := startManageServer(t, `{"account":"alice","created":true,"pending":true}`, 0)
	m.refuse = -1
	root := newTestRoot(t)
	root.SetArgs([]string{"remote", "create", "ws", "--no-context", "--token", pendingInvite(t, m)})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := root.ExecuteContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("after %v: %v", time.Since(start), err)
	}
}
