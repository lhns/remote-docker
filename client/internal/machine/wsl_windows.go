//go:build windows

package machine

// The WSL backend: the part that runs wsl.exe.
//
// Everything it decides lives in wsl.go and is tested on any platform. What is
// here is argument assembly and process running, kept as thin as it can be
// made, because this is the code that ships without anybody having run it.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func init() { registered = append(registered, wslBackend{}) }

type wslBackend struct{}

func (wslBackend) Name() string { return "wsl" }

// wsl runs wsl.exe and returns its output, decoded.
func (wslBackend) wsl(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "wsl.exe", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("wsl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(decodeWSLOutput(out)))
	}
	return out, nil
}

func (b wslBackend) Available(ctx context.Context) error {
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return fmt.Errorf("WSL is not installed on this machine\n" +
			"  fix: `wsl --install` in an administrator terminal, then reboot")
	}
	if _, err := b.wsl(ctx, "--status"); err != nil {
		return fmt.Errorf("WSL is installed but not usable: %w", err)
	}
	return nil
}

func (b wslBackend) Inspect(ctx context.Context, name string) (Observed, error) {
	distro := WSLName(name)

	// The exit status is deliberately not consulted. WSL exits NON-ZERO when
	// there are no distributions at all -- "Windows Subsystem for Linux has no
	// installed distributions" -- which is not a failure to look, it is the
	// answer, and treating it as a failure made the very first `machine create`
	// on a fresh WSL refuse with "cannot tell what is there".
	//
	// A real failure to reach WSL was already caught by Available, which ran
	// immediately before this and asked the service directly.
	raw, _ := b.wsl(ctx, "--list", "--verbose")

	observed := observeWSL(parseWSLList(raw), distro, "")
	if observed.State == Absent {
		return observed, nil
	}

	// Read from inside the distribution, so that a machine somebody exported
	// and re-imported carries its own answer. A read that fails leaves the
	// generation empty, which Plan treats as a match rather than a mismatch --
	// deliberately, because destroying a machine over an unreadable file would
	// take somebody's containers with it.
	if gen, err := b.wsl(ctx, wslReadGenerationArgs(distro)...); err == nil {
		observed.Generation = strings.TrimSpace(decodeWSLOutput(gen))
	}
	return observed, nil
}

// Create imports a rootfs as a distribution and arranges for the agent to run
// in it.
//
// The rootfs is the workspace image's filesystem, which is the whole reason
// this is short: the thing being installed is the artifact CI builds and tests
// on every push, and there is no package manager anywhere on this path.
func (b wslBackend) Create(ctx context.Context, spec Spec) error {
	if spec.Rootfs == "" {
		return fmt.Errorf("no rootfs to import: a machine is created from the workspace image's filesystem")
	}
	distro := WSLName(spec.Name)

	dir, err := wslStateDir(spec.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("preparing %s: %w", dir, err)
	}

	if _, err := b.wsl(ctx, wslImportArgs(distro, dir, spec.Rootfs, 2)...); err != nil {
		return err
	}

	// Everything below is written INTO the distribution, so a rebuild starts
	// from the same rootfs and applies the same settings, and nothing about
	// the machine lives only in this program's memory.
	for _, args := range [][]string{
		wslRunArgs(distro, "mkdir", "-p",
			"/etc/workspace/authorized_keys.d", "/etc/workspace/host_keys"),
		wslWriteArgs(distro, generationFile, spec.Generation()),
		wslWriteArgs(distro, "/etc/wsl.conf", wslConf(spec)),
	} {
		if _, err := b.wsl(ctx, args...); err != nil {
			return err
		}
	}

	// The boot command only takes effect on a fresh start, and the import
	// leaves the distribution running.
	if _, err := b.wsl(ctx, "--terminate", distro); err != nil {
		return err
	}
	return b.Start(ctx, spec.Name)
}

// Enrol writes a public key where the agent's watcher will find it.
//
// The filename is the account name, which is the enrolment convention
// everywhere else (ADR 0010), and the agent polls the directory as well as
// watching it, so a key written into a running machine is picked up without
// anything being restarted.
func (b wslBackend) Enrol(ctx context.Context, name, account, publicKey string) error {
	path := "/etc/workspace/authorized_keys.d/" + account + ".pub"
	// Backquoted, so the \n reaches printf as two characters for IT to
	// interpret. A Go "\n" here would put a real newline in the middle of the
	// shell command, which happens to work and reads like a mistake.
	_, err := b.wsl(ctx, wslRunArgs(WSLName(name),
		"sh", "-c", `printf '%s\n' `+shellQuote(strings.TrimSpace(publicKey))+" > "+path)...)
	return err
}

// Start runs the distribution, which runs its boot command.
//
// `wsl -d <name> true` is the whole of it: WSL starts a distribution on first
// use and there is no separate start verb.
func (b wslBackend) Start(ctx context.Context, name string) error {
	_, err := b.wsl(ctx, wslRunArgs(WSLName(name), "true")...)
	return err
}

func (b wslBackend) Address(ctx context.Context, name string) (string, error) {
	out, err := b.wsl(ctx, wslAddressArgs(WSLName(name))...)
	if err != nil {
		return "", err
	}
	return parseWSLAddress(out), nil
}

func (b wslBackend) Stop(ctx context.Context, name string) error {
	_, err := b.wsl(ctx, "--terminate", WSLName(name))
	return err
}

// Destroy unregisters the distribution, which deletes its disk.
func (b wslBackend) Destroy(ctx context.Context, name string) error {
	_, err := b.wsl(ctx, "--unregister", WSLName(name))
	return err
}

// wslStateDir is where a distribution's disk lives.
func wslStateDir(name string) (string, error) {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return "", fmt.Errorf("LOCALAPPDATA is not set, so there is nowhere to put the machine's disk")
	}
	return filepath.Join(local, "remote-docker", "machines", name), nil
}
