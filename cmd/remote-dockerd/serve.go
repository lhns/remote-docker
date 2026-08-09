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
	"github.com/lhns/remote-docker/internal/server/daemons"
	"github.com/lhns/remote-docker/internal/server/elevate"
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

	// envPerUserDind gives each account its own dockerd (ADR 0019) instead of
	// sharing one (ADR 0012).
	//
	// Defaults to ON. Set it false for the shared daemon, which stays
	// supported rather than deprecated: a single-account workspace has nothing
	// to separate and would pay for separation in memory and in a duplicated
	// layer cache.
	//
	// Turning it on is a BREAKING upgrade for a workspace that has users --
	// images and volumes built under the shared daemon are invisible from an
	// account's own. The old data is still in the shared /var/lib/docker.
	envPerUserDind = "WORKSPACE_PER_USER_DIND"

	// envDindImage and envDindStorage tune the per-account daemons. The
	// storage driver is not inherited from the parent, so a deployment whose
	// graph volume is Ceph-backed has to say fuse-overlayfs here too.
	envDindImage   = "WORKSPACE_DIND_IMAGE"
	envDindStorage = "WORKSPACE_DIND_STORAGE_DRIVER"
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

	perUserDind := envOr(envPerUserDind, "true") == "true"

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
	dockerdArgs := supervise.SplitArgs(os.Getenv(envDockerd))
	daemon := &supervise.Dockerd{
		Args: dockerdArgs,
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

	// With a daemon per account the shared `docker` group must go: it grants
	// a socket that reaches the PARENT daemon, which holds every account's
	// dind. Revoke covers the accounts that already exist, which on an
	// upgraded workspace is all of them.
	provisioner := &accounts.UnixProvisioner{}
	if perUserDind {
		provisioner.Groups = []string{"workspace"}
		provisioner.Revoke = []string{"docker"}
	}

	store := accounts.New(keysDir, stateDir, mapping,
		provisioner, logger{prefix: "accounts"})
	store.Shell = envOr(envShell, "/bin/bash")

	if err := ensureGroups(perUserDind); err != nil {
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

	// Nil unless asked for, and nil is what keeps the shared daemon. A
	// single-account workspace has nothing to separate and would pay for
	// separation in memory and in duplicated layer cache.
	var manager *daemons.Manager
	if perUserDind {
		// The id identifies THIS workspace across redeploys, which a container
		// id cannot. Without it the daemons are still labelled as ours, just
		// not as ours-in-particular, so two workspaces sharing a parent daemon
		// would adopt each other's.
		id, err := daemons.WorkspaceID(stateDir)
		if err != nil {
			return err
		}

		// Inherited from the workspace's own dockerd unless overridden. A
		// deployment on Ceph- or NFS-backed storage sets fuse-overlayfs there,
		// and a per-account daemon whose graph volume lives on that same
		// filesystem needs the same answer -- otherwise dockerd falls back to
		// vfs, which copies the whole image on every container create and says
		// nothing about why everything became slow.
		storage := os.Getenv(envDindStorage)
		if storage == "" {
			storage = daemons.StorageDriverFrom(dockerdArgs)
			if storage != "" {
				log.Printf("per-account daemons inherit --storage-driver=%s", storage)
			}
		}

		// The workspace's OWN image by default, because it is the only one
		// known to carry what this workspace decided it needs -- fuse-overlayfs
		// above all, which stock docker:dind does not ship. elevate sets
		// WORKSPACE_IMAGE from the container it inspected; a deployment that
		// does not elevate sets it in the stack file. Without either, the
		// stock image is used and a Ceph- or NFS-backed workspace will say so
		// loudly rather than silently.
		image := os.Getenv(envDindImage)
		if image == "" {
			image = os.Getenv(elevate.ImageEnv)
		}
		if image != "" {
			log.Printf("per-account daemons run %s", image)
		}

		manager = &daemons.Manager{
			Options: daemons.Options{
				Workspace:     id,
				Image:         image,
				StorageDriver: storage,
			},
			Log: logger{prefix: "daemons"}.Printf,
		}
		log.Printf("each account gets its own docker daemon (workspace %s)", id)

		// Adopt before serving. A restarted agent that did not would find
		// every name taken -- `docker run --name` conflicts rather than
		// replacing -- and every account locked out of the daemon holding its
		// own running containers.
		if n, err := manager.Adopt(ctx); err != nil {
			log.Printf("could not adopt existing daemons: %v", err)
		} else if n > 0 {
			log.Printf("adopted %d running daemon(s) from a previous run", n)
		}
	}

	server, err := sshd.New(sshd.Config{
		Addr:     addr,
		HostKeys: hostKeys,
		Accounts: store,
		Mapping:  mapping,
		Daemons:  manager,
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
func ensureGroups(perUserDind bool) error {
	groups := []string{"docker", "workspace"}
	if perUserDind {
		// The `docker` group is not created, because nothing should be in it.
		// It may still EXIST on an upgraded workspace -- the image or an
		// earlier run made it -- which is why accounts are revoked from it
		// rather than the group being assumed absent.
		groups = []string{"workspace"}
	}
	for _, group := range groups {
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
