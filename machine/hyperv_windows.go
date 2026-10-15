//go:build windows

package machine

// The Hyper-V executor: the part that runs powershell.exe.
//
// Everything it decides lives in hyperv.go and is tested on any platform. What
// is here is process running, kept thinner than the WSL backend's because that
// one at least runs in CI and this one runs in none (ADR 0026).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func init() { registered = append(registered, hyperVBackend{exec: newDirectHyperV()}) }

// directHyperV runs every operation as PowerShell in this process's session,
// with this process's rights.
type directHyperV struct {
	// run executes one script with stdin and returns its output. A field so
	// the test can record the scripts; powershell is the only real one.
	run func(ctx context.Context, script, stdin string) (string, error)

	// dir is where a machine's files go. Nil means the user's profile
	// (stateDir); the service sets its own ProgramData one.
	dir func(name string) (string, error)
}

func (d directHyperV) stateDir(name string) (string, error) {
	if d.dir != nil {
		return d.dir(name)
	}
	return stateDir(name)
}

func newDirectHyperV() directHyperV { return directHyperV{run: powershell} }

// powershell runs one script, wrapped by psScript, and returns its output.
//
// -NoProfile because a user's profile can print banners into what is parsed
// here, and -NonInteractive so a cmdlet that wants confirmation fails instead
// of waiting for a keystroke nobody will send. stdin is for data that must not
// be on a command line.
// powershellPath is Windows PowerShell by its full path under the system
// directory, never found through PATH: the service runs this as SYSTEM, and a
// PATH entry a user can write to would be run as SYSTEM.
func powershellPath() (string, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	exe := filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(exe); err != nil {
		return "", errors.New("PowerShell is not available, so Hyper-V cannot be managed from here")
	}
	return exe, nil
}

func powershell(ctx context.Context, script, stdin string) (string, error) {
	exe, err := powershellPath()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, exe,
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", psScript(script))
	cmd.Stdin = strings.NewReader(stdin)

	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("%w: %s", err, text)
	}
	return text, nil
}

func (d directHyperV) ps(ctx context.Context, script string) (string, error) {
	return d.run(ctx, script, "")
}

// Available reports why Hyper-V cannot be used, or nil.
//
// Two questions with different fixes: Hyper-V may not be installed, which needs
// a Windows edition that has it and a reboot; or it may be installed and this
// program not elevated. Reported rather than elevated silently, which is where
// ADR 0026 records that the project's "nothing needs to be installed" premise
// stops being true.
func (d directHyperV) Available(ctx context.Context) error {
	if _, err := powershellPath(); err != nil {
		return err
	}

	out, err := d.ps(ctx, "(Get-Command Get-VM -ErrorAction SilentlyContinue) -ne $null")
	if err != nil || !strings.EqualFold(strings.TrimSpace(out), "True") {
		return errors.New("Hyper-V is not enabled on this Windows\n" +
			"  fix: `Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V -All` in an administrator terminal, then reboot")
	}

	// Asked of Hyper-V rather than of the token, because "is this process an
	// administrator" and "may this process manage virtual machines" are not the
	// same question: Hyper-V Administrators is a group that grants the second
	// without the first.
	out, err = d.ps(ctx, "try { Get-VM -ErrorAction Stop | Out-Null; 'yes' } catch { 'no' }")
	if err != nil || !strings.EqualFold(strings.TrimSpace(out), "yes") {
		return errors.New("this terminal may not manage Hyper-V machines\n" +
			"  fix: run it as administrator, or add yourself to the Hyper-V Administrators group and sign in again")
	}
	return nil
}

// Look reads a machine's state and its recorded notes, in one call (psGetVM) so
// the two cannot disagree. A machine that is not there prints nothing.
func (d directHyperV) Look(ctx context.Context, name string) (State, hyperVNotes, error) {
	out, err := d.ps(ctx, psGetVM(machineName(name)))
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

// Create builds the machine from a Flatcar disk and one Ignition document,
// handed to it over KVP before its first boot (psAddKVP).
func (d directHyperV) Create(ctx context.Context, spec Spec) error {
	if spec.Rootfs == "" {
		return errors.New("no disk to build from: a Hyper-V machine is created from a Flatcar disk image\n" +
			"  fix: omit --rootfs to download one, or pass a downloaded .vhdx")
	}

	// Before the copy, which is over a gigabyte.
	if strings.TrimSpace(spec.PublicKey) == "" {
		return errors.New("no key to enrol: a Hyper-V machine has no way in but the key it is built with")
	}
	config, err := ignition(spec, spec.PublicKey)
	if err != nil {
		return err
	}
	input, err := kvpInput(ignitionKVP(config))
	if err != nil {
		return err
	}

	dir, err := d.stateDir(spec.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("preparing %s: %w", dir, err)
	}

	// Copied, not used in place. The machine writes to its disk, and destroying
	// the file the next machine is built from is a surprise worth the disk
	// space.
	vhd := filepath.Join(dir, "disk.vhdx")
	if err := copyFile(spec.Rootfs, vhd); err != nil {
		return fmt.Errorf("copying the disk image: %w", err)
	}

	if _, err := d.ps(ctx, psNewVM(machineName(spec.Name), vhd, dir, spec)); err != nil {
		return fmt.Errorf("creating the machine: %w", err)
	}
	if _, err := d.run(ctx, psAddKVP(machineName(spec.Name)), input); err != nil {
		return fmt.Errorf("handing the machine its configuration: %w", err)
	}

	// The single point at which a machine becomes "built": recorded after
	// creation, so one that failed halfway is not marked as built with a key it
	// never received, nor as built from settings it only half matches. See
	// hyperVBuilding for what it carries until then.
	notes := hyperVNotes{Generation: spec.Generation(), Key: keyFingerprint(spec.PublicKey)}
	if _, err := d.ps(ctx, psSetNotes(machineName(spec.Name), notes)); err != nil {
		return err
	}
	return d.Start(ctx, spec.Name)
}

func (d directHyperV) Retract(ctx context.Context, name string) error {
	_, err := d.ps(ctx, psRemoveKVP(machineName(name)))
	return err
}

func (d directHyperV) Start(ctx context.Context, name string) error {
	_, err := d.ps(ctx, psStart(machineName(name)))
	return err
}

func (d directHyperV) Address(ctx context.Context, name string) (string, error) {
	out, err := d.ps(ctx, psAddress(machineName(name)))
	if err != nil {
		return "", err
	}
	return parseVMAddress(out), nil
}

// stopGrace is how long a graceful stop may take before the machine is turned
// off. A guest that ignores the shutdown request would otherwise hold the
// operation, and with the service the machine's name, until the context ends.
var stopGrace = 90 * time.Second

func (d directHyperV) Stop(ctx context.Context, name string) error {
	gctx, cancel := context.WithTimeout(ctx, stopGrace)
	defer cancel()
	_, err := d.ps(gctx, psStop(machineName(name)))
	if err == nil || ctx.Err() != nil {
		return err
	}
	// Graceful did not finish. The guest's docker state is journalled; a
	// machine that cannot be stopped is worse.
	_, err = d.ps(ctx, psStopForce(machineName(name)))
	return err
}

func (d directHyperV) Destroy(ctx context.Context, name string) error {
	dir, err := d.stateDir(name)
	if err != nil {
		return err
	}
	_, err = d.ps(ctx, psRemoveVM(machineName(name), dir))
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
