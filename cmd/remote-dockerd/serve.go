package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/internal/server/accounts"
	"github.com/lhns/remote-docker/internal/server/mount"
	"github.com/lhns/remote-docker/internal/server/notify"
	"github.com/lhns/remote-docker/internal/server/sshd"
	"github.com/lhns/remote-docker/internal/server/supervise"
	"github.com/lhns/remote-docker/pkg/workspace"
)

// Environment variables the workspace image sets, kept compatible with the
// shell implementation so an existing deployment needs no change.
const (
	envStateDir   = "WORKSPACE_STATE_DIR"
	envKeysDir    = "WORKSPACE_KEYS_DIR"
	envHostKeys   = "WORKSPACE_HOSTKEY_DIR"
	envUIDBase    = "WORKSPACE_UID_BASE"
	envPortBase   = "WORKSPACE_PORT_BASE"
	envShell      = "WORKSPACE_SHELL"
	envDockerd    = "WORKSPACE_DOCKERD_ARGS"
	envEnableDind = "WORKSPACE_ENABLE_DIND"
	envPollSecs   = "WORKSPACE_KEY_POLL_INTERVAL"
)

func newServeCommand() *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the workspace agent",
		Long: `Runs as PID 1 in the workspace container: starts and supervises dockerd,
provisions an account per enrolled public key, and serves SSH.

Replaces sshd, the key watcher, the mount helpers and sudo.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(addr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":2222", "address to serve SSH on")
	return cmd
}

func serve(addr string) error {
	log := logger{prefix: "workspace"}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stateDir := envOr(envStateDir, "/etc/workspace")
	keysDir := envOr(envKeysDir, filepath.Join(stateDir, "authorized_keys.d"))
	hostKeyDir := envOr(envHostKeys, filepath.Join(stateDir, "host_keys"))

	for _, dir := range []string{stateDir, keysDir, hostKeyDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	mapping := workspace.Mapping{
		UIDBase:  envInt(envUIDBase, workspace.DefaultUIDBase),
		PortBase: envInt(envPortBase, workspace.DefaultPortBase),
	}
	if err := mapping.Validate(); err != nil {
		return err
	}

	var wg sync.WaitGroup

	// dockerd first: an account can be provisioned without it, but the
	// workspace is not much use until the daemon is up, and starting it early
	// overlaps its boot with everything else.
	daemon := &supervise.Dockerd{
		Args: supervise.SplitArgs(os.Getenv(envDockerd)),
		Log:  logger{prefix: "dockerd"},
	}
	if envOr(envEnableDind, "true") == "true" {
		wg.Go(func() {
			if err := daemon.Run(ctx); err != nil {
				log.Printf("dockerd supervisor stopped: %v", err)
			}
		})
		if err := daemon.WaitReady(ctx); err != nil {
			// Not fatal. A workspace that serves without a daemon lets someone
			// log in and see what went wrong, which beats a container that
			// exits and takes the evidence with it.
			log.Printf("%v; serving anyway", err)
		}
	}

	store := accounts.New(keysDir, stateDir, mapping,
		&accounts.UnixProvisioner{}, logger{prefix: "accounts"})
	store.Shell = envOr(envShell, "/bin/bash")

	if err := ensureGroups(); err != nil {
		log.Printf("%v", err)
	}

	wg.Go(func() {
		poll := time.Duration(envInt(envPollSecs, 60)) * time.Second
		if err := store.Watch(ctx, poll); err != nil {
			log.Printf("account watcher stopped: %v", err)
		}
	})

	hostKeys, err := loadHostKeys(hostKeyDir)
	if err != nil {
		return err
	}

	server, err := sshd.New(sshd.Config{
		Addr:     addr,
		HostKeys: hostKeys,
		Accounts: store,
		Mapping:  mapping,
		Mounts:   mount.New(logger{prefix: "mount"}),
		Volumes:  notify.DockerVolumes{},
		Version:  version,
		Log:      logger{prefix: "sshd"},
	})
	if err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	wg.Go(func() { serveErr <- server.Serve(ctx) })

	select {
	case <-ctx.Done():
		log.Printf("shutting down")
	case err := <-serveErr:
		if err != nil {
			stop()
			wg.Wait()
			return err
		}
	}

	_ = server.Close()
	_ = daemon.Stop()
	wg.Wait()
	return nil
}

// ensureGroups creates the groups accounts are added to.
func ensureGroups() error {
	for _, group := range []string{"docker", "workspace"} {
		if err := addGroup(group); err != nil {
			return fmt.Errorf("creating group %s: %w", group, err)
		}
	}
	return nil
}

// loadHostKeys reads the workspace's host keys, generating them on first run.
//
// Kept on the state volume so clients do not get a changed-host-key warning
// every time the container is recreated -- which they would learn to click
// through, which is worse than not warning at all.
func loadHostKeys(dir string) ([]ssh.Signer, error) {
	var signers []ssh.Signer

	path := filepath.Join(dir, "ssh_host_ed25519_key")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if data, err = generateHostKey(path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("reading host key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parsing host key %s: %w", path, err)
	}
	return append(signers, signer), nil
}

func generateHostKey(path string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "remote-docker workspace")
	if err != nil {
		return nil, fmt.Errorf("marshalling host key: %w", err)
	}
	encoded := pem.EncodeToMemory(block)

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, fmt.Errorf("writing host key: %w", err)
	}
	return encoded, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envInt(name string, fallback int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// logger prefixes messages so the container log says which part spoke.
type logger struct{ prefix string }

func (l logger) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, "["+l.prefix+"] "+format+"\n", args...)
}
