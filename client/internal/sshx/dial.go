package sshx

// The adapter between this project's auth and the transport that carries it.
//
// pkg/tunnel/client takes a signer and a host key callback and decides nothing
// about either (ADR 0030), and host/keys produces both without knowing what
// they will authenticate to. This is where the two meet.
//
// It is also the only place that can attach the enrolment hint, and that is the
// reason the boundary falls here rather than one function further in. The hint
// names a file in the workspace's authorized_keys.d, which is this project's
// convention for who may log in -- the transport neither knows it nor should.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-client/keys"
	tunnelclient "github.com/lhns/remote-docker/pkg/tunnel/client"
)

// Client and Forward are the transport's, named here because this package is
// how the rest of the client reaches them.
type (
	Client  = tunnelclient.Client
	Forward = tunnelclient.Forward
)

// Config describes how to reach a workspace, in this project's terms: a keypair
// on disk and a known_hosts file, rather than the two values they produce.
type Config struct {
	Host string
	Port int
	User string

	Key        keys.KeyPair
	KnownHosts *keys.KnownHosts
}

// Dial connects and authenticates, and says what to do about a refusal.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.KnownHosts == nil {
		return nil, errors.New("sshx: Config.KnownHosts is required")
	}
	if cfg.Key.Signer == nil {
		return nil, errors.New("sshx: Config.Key is required")
	}

	c, err := tunnelclient.Dial(ctx, tunnelclient.Config{
		Host:    cfg.Host,
		Port:    cfg.Port,
		User:    cfg.User,
		Signer:  cfg.Key.Signer,
		HostKey: cfg.KnownHosts.Callback(),
	})
	if err != nil {
		if hint := enrolmentHint(err, cfg); hint != "" {
			return nil, fmt.Errorf("%w%s", err, hint)
		}
		return nil, err
	}
	return c, nil
}

// enrolmentHint is what to add to an authentication failure, and nothing at
// all for any other kind.
//
// The workspace enrols a key by filename, out of band, so "unable to
// authenticate" is nearly always a key that has not been put there yet or a
// file that has just been written and not yet read. Neither the account nor the
// file nor the key is in the error, and all three are needed to fix it.
//
// Matched on x/crypto's wording, which is not a promise it makes. A reworded
// upstream costs the hint and leaves the error, which is the right way round.
func enrolmentHint(err error, cfg Config) string {
	if err == nil || cfg.Key.Signer == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		return ""
	}
	return fmt.Sprintf(
		"\n  fix: enrol this key as authorized_keys.d/%s.pub; it is read within a minute\n  key: %s",
		cfg.User, ssh.FingerprintSHA256(cfg.Key.Signer.PublicKey()))
}
