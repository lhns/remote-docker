package sshx

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// KnownHosts implements trust-on-first-use host key checking against a
// known_hosts file, which is what StrictHostKeyChecking=accept-new did for the
// shell clients.
//
// First contact with a host records its key. Every later connection must match
// it. An unknown host is a normal event; a *changed* host key is not, and is
// refused rather than prompted for -- there is no interactive user on the far
// side of an automated tunnel to make that judgement.
type KnownHosts struct {
	Path string

	mu sync.Mutex
}

// NewKnownHosts returns a checker backed by path, creating the file if it does
// not exist so the first connection has somewhere to record its trust.
func NewKnownHosts(path string) (*KnownHosts, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("sshx: creating known_hosts directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sshx: opening known_hosts: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sshx: closing known_hosts: %w", err)
	}
	return &KnownHosts{Path: path}, nil
}

// Callback returns an ssh.HostKeyCallback enforcing the policy above.
func (k *KnownHosts) Callback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		k.mu.Lock()
		defer k.mu.Unlock()

		// Reloaded per connection rather than cached: another process, or
		// another connection from this one -- may have recorded a host since
		// the checker was built.
		check, err := knownhosts.New(k.Path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("sshx: reading known_hosts: %w", err)
			}
			check = func(string, net.Addr, ssh.PublicKey) error {
				return &knownhosts.KeyError{}
			}
		}

		err = check(hostname, remote, key)
		if err == nil {
			return nil
		}

		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err
		}

		// A KeyError with no Want entries means "host not in the file".
		// With Want entries it means "host is in the file, with a different
		// key", which is the case that must never be papered over.
		if len(keyErr.Want) > 0 {
			return fmt.Errorf(
				"sshx: host key for %s has CHANGED.\n"+
					"This is either a reinstalled workspace or an interception.\n"+
					"If the workspace was genuinely rebuilt without its persisted host keys,\n"+
					"remove the entry for %s from %s and reconnect.\n"+
					"offered key: %s",
				hostname, hostname, k.Path, ssh.FingerprintSHA256(key))
		}

		return k.trust(hostname, remote, key)
	}
}

// trust records a first-contact host key.
func (k *KnownHosts) trust(hostname string, remote net.Addr, key ssh.PublicKey) error {
	addrs := []string{knownhosts.Normalize(hostname)}
	if remote != nil {
		if normalized := knownhosts.Normalize(remote.String()); normalized != addrs[0] {
			addrs = append(addrs, normalized)
		}
	}

	f, err := os.OpenFile(k.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("sshx: recording host key: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintln(f, knownhosts.Line(addrs, key)); err != nil {
		return fmt.Errorf("sshx: recording host key: %w", err)
	}
	return nil
}
