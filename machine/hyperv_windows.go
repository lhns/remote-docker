//go:build windows

package machine

// The Hyper-V backend: the part that runs powershell.exe.
//
// Everything it decides lives in hyperv.go and is tested on any platform. What
// is here is command assembly and process running, kept as thin as it can be
// made -- more strictly than the WSL backend, because that one at least runs in
// CI. This does not run anywhere: GitHub's runners do not offer Hyper-V and
// nobody working on this has it, so `docs/testing-machines.md` is the whole of
// its verification.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func init() { registered = append(registered, hyperVBackend{}) }

type hyperVBackend struct{}

func (hyperVBackend) Name() string { return "hyperv" }

// ps runs one PowerShell command and returns its output.
//
// -NoProfile because a user's profile can print banners into what is parsed
// here, and -NonInteractive so a cmdlet that wants confirmation fails instead
// of waiting for a keystroke nobody will send.
func (hyperVBackend) ps(ctx context.Context, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%w: %s", err, text)
	}
	return text, nil
}

// Available reports why this backend cannot be used, or nil.
//
// Two questions, and they have different answers. Hyper-V may not be installed,
// which is a Windows edition and a feature the user has to enable and reboot
// for; or it may be installed and this program not elevated, which is where ADR
// 0026 records that the project's "nothing needs to be installed" premise stops
// being true. Reported rather than elevated silently: a tool that asks for
// administrator on its own is a tool people stop reading.
func (b hyperVBackend) Available(ctx context.Context) error {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return errors.New("PowerShell is not available, so Hyper-V cannot be managed from here")
	}

	out, err := b.ps(ctx, "(Get-Command Get-VM -ErrorAction SilentlyContinue) -ne $null")
	if err != nil || !strings.EqualFold(strings.TrimSpace(out), "True") {
		return errors.New("Hyper-V is not enabled on this Windows\n" +
			"  fix: `Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All` in an administrator terminal, then reboot")
	}

	// Asked of Hyper-V rather than of the token, because "is this process an
	// administrator" and "may this process manage virtual machines" are not the
	// same question: Hyper-V Administrators is a group that grants the second
	// without the first.
	out, err = b.ps(ctx, "try { Get-VM -ErrorAction Stop | Out-Null; 'yes' } catch { 'no' }")
	if err != nil || !strings.EqualFold(strings.TrimSpace(out), "yes") {
		return errors.New("this terminal may not manage Hyper-V machines\n" +
			"  fix: run it as administrator, or add yourself to the Hyper-V Administrators group and sign in again")
	}
	return nil
}

// look reads a machine's state and its recorded notes. See psGetVM, which asks
// for both in one call.
//
// Absent with empty notes is a machine that is not there: Get-VM with
// -ErrorAction SilentlyContinue says so by printing nothing.
func (b hyperVBackend) look(ctx context.Context, name string) (State, hyperVNotes, error) {
	out, err := b.ps(ctx, psGetVM(machineName(name)))
	if err != nil {
		return Absent, hyperVNotes{}, err
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	var notes hyperVNotes
	if len(lines) > 1 {
		notes = decodeNotes(lines[1])
	}
	return parseVMState(lines[0]), notes, nil
}

func (b hyperVBackend) Inspect(ctx context.Context, name string) (Observed, error) {
	state, notes, err := b.look(ctx, name)
	if err != nil || state == Absent {
		return Observed{State: Absent}, nil
	}
	return Observed{State: state, Generation: notes.Generation}, nil
}

// Create builds the machine from a Flatcar image and one Ignition document.
//
// The image is a file the user downloaded, named by --image, exactly as the WSL
// backend takes a rootfs. Nothing is fetched here: an installer that downloads
// is an installer that can be halfway through, which is the state this whole
// design exists to not have.
func (b hyperVBackend) Create(ctx context.Context, spec Spec) error {
	if spec.Rootfs == "" {
		return errors.New("no image to build from: a Hyper-V machine is created from a Flatcar disk image\n" +
			"  fix: see docs/testing-machines.md for the one command that downloads it")
	}

	dir, err := stateDir(spec.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("preparing %s: %w", dir, err)
	}

	// Copied, not used in place. The machine writes to its disk, and a machine
	// destroying the file the user downloaded -- which is also the file the
	// next machine is built from -- is a surprise worth the disk space.
	vhd := filepath.Join(dir, "disk.vhdx")
	if err := copyFile(spec.Rootfs, vhd); err != nil {
		return fmt.Errorf("copying the disk image: %w", err)
	}

	if strings.TrimSpace(spec.PublicKey) == "" {
		return errors.New("no key to enrol: a Hyper-V machine has no way in but the key it is built with")
	}
	config, err := ignition(spec, spec.PublicKey)
	if err != nil {
		return err
	}

	// Written beside the disk, where Flatcar's Hyper-V image reads it from the
	// host through the data-source Ignition uses on this platform.
	if err := os.WriteFile(filepath.Join(dir, "config.ign"), []byte(config), 0o600); err != nil {
		return fmt.Errorf("writing the machine's configuration: %w", err)
	}

	if _, err := b.ps(ctx, psNewVM(machineName(spec.Name), vhd, dir, spec)); err != nil {
		return fmt.Errorf("creating the machine: %w", err)
	}

	// Recorded after creation so a machine that failed halfway is not marked as
	// built with a key it never received.
	notes := hyperVNotes{Generation: spec.Generation(), Key: keyFingerprint(spec.PublicKey)}
	if _, err := b.ps(ctx, psSetNotes(machineName(spec.Name), notes)); err != nil {
		return err
	}
	return b.Start(ctx, spec.Name)
}

// Enrol reports whether this key already reaches the machine.
//
// It cannot write one. A Hyper-V machine takes its key at creation, because
// afterwards the only way in is the SSH that key is for -- PowerShell Direct is
// Windows-guest only. See hyperVEnrolment.
func (b hyperVBackend) Enrol(ctx context.Context, name, _, publicKey string) error {
	_, notes, err := b.look(ctx, name)
	if err != nil {
		return err
	}
	return hyperVEnrolment(notes, publicKey)
}

func (b hyperVBackend) Start(ctx context.Context, name string) error {
	_, err := b.ps(ctx, fmt.Sprintf("Start-VM -Name %s -ErrorAction SilentlyContinue", psQuote(machineName(name))))
	return err
}

// Hold is nothing, and that is the whole of it.
//
// A Hyper-V machine runs until it is stopped. Unlike a WSL distribution it has
// no idle timeout, so there is nothing to hold open and nothing to release.
func (hyperVBackend) Hold(context.Context, string) (io.Closer, error) {
	return closerFunc(func() error { return nil }), nil
}

func (b hyperVBackend) Address(ctx context.Context, name string) (string, error) {
	out, err := b.ps(ctx, psAddress(machineName(name)))
	if err != nil {
		return "", err
	}
	return parseVMAddress(out), nil
}

// Stop shuts the machine down and waits for it.
//
// Stop-VM without -TurnOff, so the guest flushes its docker state. -Force means
// "do not ask about signed-in users", not "pull the plug".
func (b hyperVBackend) Stop(ctx context.Context, name string) error {
	_, err := b.ps(ctx, fmt.Sprintf("Stop-VM -Name %s -Force", psQuote(machineName(name))))
	return err
}

func (b hyperVBackend) Destroy(ctx context.Context, name string) error {
	dir, err := stateDir(name)
	if err != nil {
		return err
	}
	_, err = b.ps(ctx, psRemoveVM(machineName(name), dir))
	return err
}

// copyFile copies a file, creating the destination.
func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}
