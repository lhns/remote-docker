package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core-client/keys"
	"github.com/lhns/remote-docker/core/enrol"
)

// ErrPredatesManagement is a workspace whose agent does not know
// enrol.EnrolCommand: its shell answered 127.
var ErrPredatesManagement = errors.New("this workspace's agent predates account management")

// Manage runs one account-management operation as cfg's account (ADR 0053).
// It also returns the host key the workspace offered, which an invite pins.
// A refusal is the reply's *enrol.Error.
func Manage(ctx context.Context, cfg config.Config, req enrol.Request) (enrol.Reply, ssh.PublicKey, error) {
	key, err := keys.LoadOrCreateKey(config.KeyPath(), config.KeyComment())
	if err != nil {
		return enrol.Reply{}, nil, err
	}
	rule, err := hostKeyRule(cfg)
	if err != nil {
		return enrol.Reply{}, nil, err
	}
	var offered ssh.PublicKey
	seen := func(host string, remote net.Addr, k ssh.PublicKey) error {
		if err := rule(host, remote, k); err != nil {
			return err
		}
		offered = k
		return nil
	}

	client, hold, err := dial(ctx, cfg, key.Signer, seen)
	if err != nil {
		return enrol.Reply{}, nil, err
	}
	if hold != nil {
		defer func() { _ = hold.Close() }()
	}
	defer func() { _ = client.Close() }()

	body, err := json.Marshal(req)
	if err != nil {
		return enrol.Reply{}, nil, err
	}
	out, err := client.RunInput(ctx, enrol.EnrolCommand, body)
	var exit *ssh.ExitError
	if errors.As(err, &exit) && exit.ExitStatus() == 127 {
		return enrol.Reply{}, nil, ErrPredatesManagement
	}
	if err != nil {
		return enrol.Reply{}, nil, err
	}
	var reply enrol.Reply
	if err := json.Unmarshal(out, &reply); err != nil {
		return enrol.Reply{}, nil, fmt.Errorf("reading the workspace's answer: %w", err)
	}
	if reply.Error != nil {
		return reply, offered, reply.Error
	}
	return reply, offered, nil
}
