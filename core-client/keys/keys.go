// Package keys is this client's identity to a workspace: the keypair it
// authenticates with and the host keys it will accept, wired to the transport
// in core-client/tunnelclient.
//
// It exists because the previous clients shelled out to ssh(1). That cost a
// layer of quoting on every remote command, turned errors into exit codes and
// stderr text, and split the two clients' capabilities permanently: connection
// multiplexing is an OpenSSH client feature that Win32-OpenSSH does not
// implement, so the POSIX client was fast and the Windows client was not. One
// ssh.Client carrying many channels makes that difference disappear rather
// than working around it.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// KeyPair is this machine's identity to the workspace.
type KeyPair struct {
	Signer ssh.Signer

	// Path is where the private key lives; Path+".pub" holds the public half.
	Path string
}

// AuthorizedKey returns the public key in authorized_keys form: the single
// line a user hands to whoever runs the workspace, whose filename becomes
// their unix account.
func (k KeyPair) AuthorizedKey(comment string) string {
	line := string(ssh.MarshalAuthorizedKey(k.Signer.PublicKey()))
	// MarshalAuthorizedKey appends a newline and no comment.
	line = line[:len(line)-1]
	if comment == "" {
		return line
	}
	return line + " " + comment
}

// LoadOrCreateKey returns the keypair at path, generating an ed25519 pair on
// first use.
//
// ed25519 rather than RSA: small, fast, no key-size decision to get wrong, and
// supported by every sshd this will meet. The key is never passphrase
// protected, because it authenticates an automated tunnel that must come up without
// a prompt, and a passphrase the user cannot be asked for is not a control.
func LoadOrCreateKey(path, comment string) (KeyPair, error) {
	signer, err := loadKey(path)
	switch {
	case err == nil:
		return KeyPair{Signer: signer, Path: path}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return KeyPair{}, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return KeyPair{}, fmt.Errorf("keys: creating key directory: %w", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("keys: generating key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return KeyPair{}, fmt.Errorf("keys: marshalling private key: %w", err)
	}

	// O_EXCL so a key created by a concurrent invocation is never clobbered:
	// overwriting it would silently revoke this machine's access, since the
	// public half is already enrolled on the workspace.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			signer, lerr := loadKey(path)
			if lerr != nil {
				return KeyPair{}, lerr
			}
			return KeyPair{Signer: signer, Path: path}, nil
		}
		return KeyPair{}, fmt.Errorf("keys: creating key file: %w", err)
	}
	if err := pem.Encode(f, block); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return KeyPair{}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return KeyPair{}, fmt.Errorf("keys: writing key file: %w", err)
	}

	signer, err = ssh.NewSignerFromKey(priv)
	if err != nil {
		return KeyPair{}, fmt.Errorf("keys: building signer: %w", err)
	}

	pub := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if comment != "" {
		pub = append(pub[:len(pub)-1], []byte(" "+comment+"\n")...)
	}
	if err := os.WriteFile(path+".pub", pub, 0o644); err != nil {
		return KeyPair{}, fmt.Errorf("keys: writing public key: %w", err)
	}

	return KeyPair{Signer: signer, Path: path}, nil
}

func loadKey(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("keys: parsing %s: %w", path, err)
	}
	return signer, nil
}
