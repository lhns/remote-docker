package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/core-client/tunnelclient"
	"github.com/lhns/remote-docker/core/enrol"
)

// ErrPredatesTokens is a workspace that refused a token login without the
// enrolment banner: one too old to know tokens at all.
var ErrPredatesTokens = errors.New("the workspace predates enrolment tokens")

// Redeem enrols this machine's key with an invite (ADR 0051), reaching the
// workspace as cfg says, and returns the account the key joined. account is
// the name asked for, empty to take the one the token is bound to.
//
// The host key must be the one the invite names, and is then recorded in
// known_hosts as a first connection would be, so the token stands in for trust
// on first use. A refusal is enrol.Refused, an *enrol.Error, or
// ErrPredatesTokens.
func Redeem(ctx context.Context, cfg config.Config, invite enrol.Invite, account string) (enrol.RedeemReply, error) {
	id, secret, err := enrol.ParseToken(invite.Token)
	if err != nil {
		return enrol.RedeemReply{}, enrol.ErrMalformed
	}
	key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return enrol.RedeemReply{}, err
	}
	known, err := keys.NewKnownHosts(config.KnownHostsPath())
	if err != nil {
		return enrol.RedeemReply{}, err
	}
	transport, err := cfg.Transport()
	if err != nil {
		return enrol.RedeemReply{}, err
	}
	dial, err := dialerFor(transport, cfg)
	if err != nil {
		return enrol.RedeemReply{}, err
	}

	// Both callbacks run inside Dial, on this goroutine.
	var banner bool
	var pinErr error
	client, err := tunnelclient.Dial(ctx, tunnelclient.Config{
		Host:    transport.Host,
		Port:    transport.Port,
		User:    enrol.LoginPrefix + id,
		Signer:  key.Signer,
		HostKey: pinnedHostKey(invite.HostKey, known.Callback(), &pinErr),
		Dial:    dial,
		Banner:  func(msg string) { banner = banner || enrol.IsBanner(msg) },
	})
	if err != nil {
		if pinErr != nil {
			return enrol.RedeemReply{}, pinErr
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			if banner {
				return enrol.RedeemReply{}, enrol.Refused
			}
			return enrol.RedeemReply{}, ErrPredatesTokens
		}
		return enrol.RedeemReply{}, err
	}
	defer func() { _ = client.Close() }()

	var reply enrol.RedeemReply
	req := enrol.RedeemRequest{Secret: secret, Account: account, Comment: config.KeyComment()}
	if err := exchange(ctx, client, enrol.RedeemCommand, req, &reply); err != nil {
		return enrol.RedeemReply{}, err
	}
	if reply.Error != nil {
		return reply, reply.Error
	}
	return reply, nil
}

// exchange runs cmd with req as its JSON input and decodes its JSON output
// into reply, which is the shape of both enrol commands.
func exchange(ctx context.Context, client *tunnelclient.Client, cmd string, req, reply any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out, err := client.RunInput(ctx, cmd, body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, reply); err != nil {
		return fmt.Errorf("reading the workspace's answer: %w", err)
	}
	return nil
}

// pinnedHostKey accepts only the host key an invite names, and then asks
// known_hosts, which records it or refuses one that changed. A refusal is also
// left in refused, unwrapped by the handshake.
func pinnedHostKey(pin string, known ssh.HostKeyCallback, refused *error) ssh.HostKeyCallback {
	return func(host string, remote net.Addr, offered ssh.PublicKey) error {
		// The pin first: known_hosts records what it accepts.
		var err error
		if got := ssh.FingerprintSHA256(offered); got != pin {
			err = fmt.Errorf("the workspace at %s offered host key %s, and the token is for %s\n"+
				"  fix: check the address, or ask for a new token", host, got, pin)
		} else {
			err = known(host, remote, offered)
		}
		*refused = err
		return err
	}
}
