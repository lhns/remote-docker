package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/dockercli"
	"github.com/lhns/remote-docker/agent/internal/elevate"
	"github.com/lhns/remote-docker/agent/internal/sshd"
	"github.com/lhns/remote-docker/agent/internal/supervise"
	"github.com/lhns/remote-docker/agent/internal/wslisten"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
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

// preferredPortTimeout bounds asking a machine's volumes which port they
// were built for. Generous, because it may include a cold daemon's boot, and
// paid only by a machine this workspace has no record of.
const preferredPortTimeout = 90 * time.Second

const (

	// envAccountPrefix goes in front of the unix user name, so an enrolled
	// `alice` does not take the name `alice` in the machine's own passwd file
	// (ADR 0025). The account name, the login name and the port are unchanged.
	envAccountPrefix = "WORKSPACE_ACCOUNT_PREFIX"

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

	// envDindMounts adds bind mounts to every account's daemon, for
	// configuration it can only be given as files: a daemon.json naming an
	// insecure registry, or the certificates for a registry with a private CA.
	// A workspace mounts those into its own daemon and each account's daemon
	// needs the same ones, or a pull that works on the workspace fails inside
	// every account.
	envDindMounts = "WORKSPACE_DIND_MOUNTS"
)

func newServeCommand() *cobra.Command {
	var addr, wsAddr string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the workspace agent",
		Long: `Runs as PID 1 in the workspace container: starts and supervises dockerd,
provisions an account per enrolled public key, and serves SSH.

Replaces sshd, the key watcher, the mount helpers and sudo.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(addr, wsAddr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":2222", "address to serve SSH on")

	// On by default, and empty turns it off. Off by default would mean every
	// existing workspace needs a redeploy before a client can reach it through
	// a proxy, which is most of the difficulty this exists to remove -- and it
	// is not a weaker door, because the same SSH handshake runs inside it.
	cmd.Flags().StringVar(&wsAddr, "ws-addr", ":2280",
		"address to serve SSH over a WebSocket on; empty disables it")
	return cmd
}

func serve(addr, wsAddr string) error {
	log := logger("workspace")

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
		Log:  logger("dockerd"),
	}
	if envOr(envEnableDind, "true") == "true" {
		wg.Go(func() {
			if err := daemon.Run(ctx); err != nil {
				log.Warn("the dockerd supervisor stopped", "err", err)
			}
		})
		if err := daemon.WaitReady(ctx); err != nil {
			// Not fatal. A workspace that serves without a daemon lets someone
			// log in and see what went wrong, which beats a container that
			// exits and takes the evidence with it.
			log.Warn("serving anyway", "err", err)
		}
	}

	// With a daemon per account the shared `docker` group must go: it grants
	// a socket that reaches the PARENT daemon, which holds every account's
	// dind. Revoke covers the accounts that already exist, which on an
	// upgraded workspace is all of them.
	// Stated in both modes, because membership is reconciled to it on every
	// pass. An empty list would mean "reconcile to nothing" rather than
	// "leave alone".
	provisioner := &accounts.UnixProvisioner{
		Groups: []string{"docker", "workspace"},
		Prefix: envOr(envAccountPrefix, accounts.DefaultPrefix),
	}
	if perUserDind {
		provisioner.Groups = []string{"workspace"}
		provisioner.Revoke = []string{"docker"}
	}

	store := accounts.New(keysDir, stateDir, mapping,
		provisioner, logger("accounts"))
	store.Shell = envOr(envShell, "/bin/bash")

	if err := ensureGroups(perUserDind); err != nil {
		log.Warn(err.Error())
	}

	wg.Go(func() {
		poll := time.Duration(envInt(envPollSecs, 60)) * time.Second
		if err := store.Watch(ctx, poll); err != nil {
			log.Warn("the account watcher stopped", "err", err)
		}
	})

	hostKeys, err := loadHostKeys(hostKeyDir)
	if err != nil {
		return err
	}

	// THE one place the mode is chosen. Everything downstream asks the resolver
	// and is told; nothing else branches on which arrangement this workspace
	// runs, which is what stops one session being routed to another account's
	// daemon by a check somebody forgot to copy (ADR 0019).
	//
	// The shared daemon is a supported configuration rather than a fallback: a
	// single-account workspace has nothing to separate and would pay for
	// separation in memory and in duplicated layer cache.
	// Parsed in BOTH modes: per-account it also performs the mounts, shared it
	// only declares what the workspace's own daemon already has (ADR 0041).
	extraMounts, err := daemons.ParseMounts(os.Getenv(envDindMounts))
	if err != nil {
		return fmt.Errorf("%s: %w", envDindMounts, err)
	}
	if missing := daemons.MissingSources(extraMounts, func(p string) error {
		_, err := os.Stat(p)
		return err
	}); len(missing) > 0 {
		return fmt.Errorf("%s: %s is not on this machine", envDindMounts, missing[0].Source)
	}
	if !perUserDind {
		for _, m := range daemons.UnmountedRemaps(extraMounts) {
			log.Warn("a shared daemon mounts nothing, so only the source is offered to binds",
				"source", m.Source, "ignored-destination", m.Destination)
		}
	}
	daemonPaths := daemons.DaemonPaths(extraMounts, perUserDind)
	if len(daemonPaths) > 0 {
		log.Info("binds may name the daemon's own paths", "paths", strings.Join(daemonPaths, ","))
	}

	targets := daemons.Shared("")
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
		// filesystem needs the same answer, or dockerd falls back to
		// vfs, which copies the whole image on every container create and says
		// nothing about why everything became slow.
		if len(extraMounts) > 0 {
			log.Info("per-account daemons get extra mounts", "count", len(extraMounts))
		}

		storage := os.Getenv(envDindStorage)
		if storage == "" {
			storage = daemons.StorageDriverFrom(dockerdArgs)
			if storage != "" {
				log.Info("per-account daemons inherit a storage driver", "driver", storage)
			}
		}

		// The workspace's OWN image by default, because it is the only one
		// known to carry what this workspace decided it needs, fuse-overlayfs
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
			log.Info("per-account daemons run an image", "image", image)
		}

		manager := &daemons.Manager{
			Options: daemons.Options{
				Workspace:     id,
				Image:         image,
				StorageDriver: storage,
				Mounts:        extraMounts,
			},
			Log: logger("daemons"),

			// The store is the authority on an account's uid: it allocated it,
			// and it knows the unix user behind the name. Asking the passwd
			// file for the ACCOUNT name would find nothing now that the unix
			// user is prefixed.
			IDs: func(account string) (int, int, error) {
				a, ok := store.Lookup(account)
				if !ok {
					return 0, 0, fmt.Errorf("no such account: %s", account)
				}
				return a.UID, a.GID, nil
			},
		}
		targets = manager
		log.Info("each account gets its own docker daemon", "workspace", id)

		// Adopt before serving. A restarted agent that did not would find
		// every name taken, so `docker run --name` conflicts rather than
		// replacing, and every account locked out of the daemon holding its
		// own running containers.
		if n, err := manager.Adopt(ctx); err != nil {
			log.Warn("could not adopt existing daemons", "err", err)
		} else if n > 0 {
			log.Info("adopted running daemons from a previous run", "count", n)
		}
	} else {
		// A workspace that has run with WORKSPACE_PER_USER_DIND=true still has
		// those daemons, and nothing here routes to them. See StopStrays.
		//
		// The id is read and never created: without one this workspace has
		// never run a per-account daemon, so there is nothing to find.
		if id, ok := daemons.KnownWorkspaceID(stateDir); ok {
			strays := &daemons.Manager{
				Options: daemons.Options{Workspace: id},
				Log:     logger("daemons"),
			}
			if n, err := strays.StopStrays(ctx); err != nil {
				log.Warn("could not check for per-account daemons left running", "err", err)
			} else if n > 0 {
				log.Info("stopped per-account daemons this mode does not use", "count", n)
			}
		}
	}

	// One port per MACHINE rather than per account (ADR 0029). The uid still
	// decides the first, so a workspace reached from one computer is on the
	// port it always was and allocates nothing.
	//
	// preferredPortTimeout bounds asking a machine's volumes which port they
	// need. Generous, because it may include a cold daemon's boot, and paid
	// only by a machine this workspace has no record of.
	ports := &accounts.Ports{
		Dir:     stateDir,
		Mapping: mapping,
		// An account that exists is entitled to the port its uid derives,
		// whether or not it has ever connected, so an allocation must not take
		// it. Asked of the store rather than remembered, because accounts come
		// and go while this file does not.
		Reserved: func(uid int) bool {
			for _, a := range store.List() {
				if a.UID == uid {
					return true
				}
			}
			return false
		},
		// The port a machine's volumes were built for, which outlives the
		// record above: a volume keeps its port forever and cannot be
		// re-pointed, so a machine given a different one loses all of them.
		//
		// Ensure, where the other info queries deliberately use Lookup. Those
		// fill in fields that are displayed, so an unavailable daemon costs a
		// dash on a table; this one is ACTED UPON and a wrong answer costs
		// somebody their volumes. It is reached only for a machine the record
		// has forgotten, so the wait is paid once by that machine and never on
		// an ordinary connect.
		Preferred: func(account, client string) (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), preferredPortTimeout)
			defer cancel()

			return dockercli.ClientPorts{
				Host: func(account string) (string, error) {
					target, err := targets.Ensure(ctx, account)
					return target.Host, err
				},
			}.For(ctx, account, client)
		},
	}

	server, err := sshd.New(sshd.Config{
		Addr:     addr,
		HostKeys: hostKeys,
		Accounts: store,
		Mapping:  mapping,
		Ports:    ports,
		Daemons:  targets,
		Version:  version,

		DaemonPaths: daemonPaths,
		Log:         logger("sshd"),
	})
	if err != nil {
		return err
	}

	serveErr := make(chan error, 1)
	wg.Go(func() { serveErr <- server.Serve(ctx) })

	// A second listener for the same SSH server, so a reverse proxy can front
	// the workspace (ADR 0034). Disabled by setting --ws-addr to "".
	if wsAddr != "" {
		ws, err := wslisten.New(wsAddr, logger("wslisten"))
		if err != nil {
			stop()
			wg.Wait()
			return fmt.Errorf("serving the websocket on %s: %w", wsAddr, err)
		}
		defer func() { _ = ws.Close() }()

		log.Info("websocket listening on " + ws.Addr() + " (any path)")
		wg.Go(func() {
			if err := server.ServeListener(ws.Listener); err != nil {
				log.Warn("the websocket listener stopped", "err", err)
			}
		})
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
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
		// It may still EXIST on an upgraded workspace, since the image or an
		// earlier run made it, which is why accounts are revoked from it
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
// every time the container is recreated, which they would learn to click
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
//
// The prefix travels as an ordinary slog attribute and is rendered by
// logx.Handler, so a subsystem asks for its own by naming itself, nothing
// carries a second logging concept, and the line on screen is what it always
// was.
func logger(component string) *slog.Logger {
	return logx.Logger(os.Stderr, "", true).With(logx.ComponentKey, component)
}
