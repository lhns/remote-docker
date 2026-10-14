package session

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core-client/keys"
)

// Variables so tests can shorten them.
var (
	// AccountPollInterval is the wait between login attempts.
	AccountPollInterval = 3 * time.Second
	// AccountWaitLimit bounds the whole wait.
	AccountWaitLimit = 4 * time.Minute
)

// ErrAccountNotReady is an account that never began to accept this machine's
// key within AccountWaitLimit.
var ErrAccountNotReady = errors.New("the workspace has not finished creating the account")

// WaitForAccount logs in as cfg's account with this machine's key until it
// works, for a redeem whose reply said pending (ADR 0051). A refused login is
// the account not existing yet and is retried; any other error ends the wait.
func WaitForAccount(ctx context.Context, cfg config.Config) error {
	key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return err
	}
	rule, err := hostKeyRule(cfg)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, AccountWaitLimit)
	defer cancel()

	for {
		err := tryLogin(waitCtx, cfg, key, rule)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil { // the caller's, not our limit
			return ctx.Err()
		}
		if waitCtx.Err() != nil {
			return ErrAccountNotReady
		}
		// A refused login is the account not existing yet; so is a dial or
		// handshake that ran into the wait's deadline, which reports its own
		// timeout a moment before waitCtx does. The next pass through the
		// loop decides whether that was the limit.
		if !strings.Contains(err.Error(), "unable to authenticate") && !isTimeout(err) {
			return err
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(AccountPollInterval):
		}
	}
}

// isTimeout reports a deadline of any spelling: the context's, the os's, or a
// net.Error that says it timed out.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

// tryLogin is one query session: a login and the workspace-info request.
func tryLogin(ctx context.Context, cfg config.Config, key keys.KeyPair, rule ssh.HostKeyCallback) error {
	client, hold, err := dial(ctx, cfg, key.Signer, rule)
	if err != nil {
		return err
	}
	if hold != nil {
		defer func() { _ = hold.Close() }()
	}
	defer func() { _ = client.Close() }()
	_, err = readInfo(ctx, client)
	return err
}
