package main

// Putting a `docker` on PATH that is this binary.
//
// The mechanism is alias.go: a process invoked under the name docker runs the
// Docker CLI. This file only arranges for that name to exist somewhere the
// shell will find it, so renaming the downloaded binary to docker.exe is a
// complete installation on its own, and this is the tidy, reversible way to do
// the same thing.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// form is how the shim points at this binary, best first.
//
// The order is about what survives an UPGRADE, which is the only thing that
// distinguishes them once installed:
//
//   - a symlink stores a path and is resolved at run time, so replacing the
//     binary (how nearly every upgrade works) leaves it correct;
//   - a hardlink is a second name for the same data, costing nothing and
//     unable to disagree, until an upgrade REPLACES the file rather than
//     rewriting it. Measured, because the difference is not obvious: `go build
//     -o` writes in place and the hardlink follows it, while deleting and
//     recreating the binary (what an installer or an unzip does) leaves
//     the shim on the old data with nothing to say so;
//   - a copy duplicates the whole binary and goes stale identically, and is
//     the only one that has to ask first.
type form string

const (
	formSymlink  form = "symlink"
	formHardlink form = "hardlink"
	formCopy     form = "copy"
)

// shimName is the file the shell finds. The extension matters on Windows,
// where PATHEXT is what makes a file executable at all.
func shimName() string {
	if runtime.GOOS == "windows" {
		return dockerName + ".exe"
	}
	return dockerName
}

// shimDir is where the shim goes.
//
// NOT config.StateDir() on Windows, and that is a decision. os.UserConfigDir()
// there is %APPDATA%, which ROAMS: an executable placed in a roaming profile
// is synced to every machine the user logs into, including ones that have a
// real Docker installed. %LOCALAPPDATA% is where per-user executables belong.
//
// ~/.local/bin on Unix is the convention, and is usually on PATH already --
// which is why nothing here edits a shell's configuration.
func shimDir() (string, error) {
	// Cleaned, because this one comes from a person: a path typed with the
	// other platform's separators is the same directory and should be printed
	// back the way this machine spells it.
	if dir := os.Getenv("REMOTE_DOCKER_SHIM_DIR"); dir != "" {
		return filepath.Clean(dir), nil
	}
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return "", errors.New("LOCALAPPDATA is not set, so there is no per-user program directory to install into")
		}
		return filepath.Join(local, "remote-docker", "bin"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding your home directory: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func shimPath() (string, error) {
	dir, err := shimDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, shimName()), nil
}

// marker records what we installed, next to what we installed.
//
// It exists because "is this file ours?" has to be answerable WITHOUT running
// the file. A hardlink to this binary answers itself through os.SameFile, but
// a stale hardlink or a copy of an older build does not, and the only other
// way to ask a binary what it is would be to execute it, which is exactly what
// must never happen to a `docker.exe` we did not put there.
//
// It also carries whether we edited PATH, which uninstall must not guess at:
// removing an entry the user added themselves would be a worse failure than
// leaving one of ours behind.
type marker struct {
	Form      string `json:"form"`
	From      string `json:"from"`
	Version   string `json:"version"`
	AddedPATH bool   `json:"addedPath,omitempty"`
}

// markerName sits beside the shim rather than inside the state directory, so a
// directory somebody has moved or deleted takes its own bookkeeping with it.
const markerName = ".remote-docker-shim.json"

func markerPath(dir string) string { return filepath.Join(dir, markerName) }

func readMarker(dir string) (marker, bool) {
	data, err := os.ReadFile(markerPath(dir))
	if err != nil {
		return marker{}, false
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(dir string, m marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(dir), append(data, '\n'), 0o600)
}

// installed is what is at the shim's path right now.
type installed struct {
	path string

	// exists is whether anything is there at all. The three below are only
	// meaningful when it is.
	exists bool

	// ours is whether it leads back to a remote-docker binary. Anything else
	// is somebody's real docker and is never touched.
	ours bool

	form    form
	target  string // where a symlink points
	current bool   // whether it still leads to THIS binary
}

// inspectShim reads what is installed without changing anything.
func inspectShim(self string) (installed, error) {
	path, err := shimPath()
	if err != nil {
		return installed{}, err
	}
	in := installed{path: path}

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return in, nil
	}
	if err != nil {
		return in, fmt.Errorf("looking at %s: %w", path, err)
	}
	in.exists = true

	if info.Mode()&os.ModeSymlink != 0 {
		in.form = formSymlink
		target, err := os.Readlink(path)
		if err != nil {
			return in, fmt.Errorf("reading the link at %s: %w", path, err)
		}
		in.target = target
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		// A symlink is ours if it leads to a remote-docker binary at all --
		// including one that has since been deleted, which is a broken
		// installation we may fix rather than a stranger's file we may not.
		in.ours = isRemoteDockerName(target)
		in.current = sameFile(target, self)
		return in, nil
	}

	// A hardlink and a copy are both ordinary files here. os.SameFile is what
	// tells them apart: a hardlink to this binary IS this binary.
	if sameFile(path, self) {
		in.form, in.ours, in.current = formHardlink, true, true
		return in, nil
	}

	// Not this binary. It is either a stale hardlink or copy of an older
	// remote-docker, or somebody's real docker CLI. The marker is what tells
	// them apart, and the absence of one means "not ours", which errs
	// towards leaving a stranger's file alone.
	if m, ok := readMarker(filepath.Dir(path)); ok {
		in.ours = true
		in.form = form(m.Form)
		return in, nil
	}
	in.form = formCopy
	return in, nil
}

// sameFile reports whether two paths are one file, following symlinks.
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// isRemoteDockerName reports whether a path names a remote-docker binary,
// which is how a symlink's target is judged.
func isRemoteDockerName(path string) bool {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return strings.EqualFold(name, "remote-docker")
}

// installShim puts the shim in place, reporting what it did.
//
// The ladder is tried in order and each rung explains itself, because the one
// that succeeds is what the user has to live with: a symlink needs no
// maintenance, a hardlink and a copy have to be refreshed after an upgrade.
func installShim(out io.Writer, in io.Reader, self string, allowCopy bool) error {
	path, err := shimPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)

	existing, err := inspectShim(self)
	if err != nil {
		return err
	}
	switch {
	case existing.exists && !existing.ours:
		// Never clobber a docker somebody else installed. This is the
		// invariant of the whole feature: a machine may get Docker Desktop
		// tomorrow, and a shim that overwrote a real CLI is a broken machine.
		return fmt.Errorf(
			"%s exists and is not ours, so it was left alone. It is probably a real docker.\n"+
				"  fix: remove it yourself, or set REMOTE_DOCKER_SHIM_DIR to install elsewhere",
			existing.path)
	case existing.exists && existing.current:
		// PATH is the caller's business rather than this function's: `install`
		// deals with it either way, and saying it here as well reads like two
		// different facts about the same directory.
		_, _ = fmt.Fprintf(out, "already installed: %s (%s)\n", existing.path, existing.form)
		return nil
	case existing.exists:
		_, _ = fmt.Fprintf(out, "replacing the %s at %s, which points at an older build\n",
			existing.form, existing.path)
		if err := os.Remove(existing.path); err != nil {
			return fmt.Errorf("removing the old shim: %w", err)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	got, err := link(out, in, self, path, allowCopy)
	if err != nil {
		return err
	}

	// Preserving AddedPATH: a reinstall over an existing shim must not forget
	// that we are the ones who put the directory on PATH, or uninstall would
	// leave it behind for ever.
	m, _ := readMarker(dir)
	m.Form, m.From, m.Version = string(got), self, version
	if err := writeMarker(dir, m); err != nil {
		return fmt.Errorf("recording what was installed: %w", err)
	}

	_, _ = fmt.Fprintf(out, "installed: %s (%s)\n", path, got)
	if got != formSymlink {
		_, _ = fmt.Fprintf(out,
			"  note: a %s does not follow upgrades; run `shim install` again after one\n", got)
	}
	return nil
}

// link walks the ladder, saying why each rung was not available.
func link(out io.Writer, in io.Reader, self, path string, allowCopy bool) (form, error) {
	if err := os.Symlink(self, path); err == nil {
		return formSymlink, nil
	} else if runtime.GOOS == "windows" {
		// Not a failure worth reporting as one: it is the ordinary state of a
		// Windows machine, and the hardlink below is a fine answer.
		_, _ = fmt.Fprintln(out,
			"  note: symlinks need Developer Mode or an administrator here, so this is a hardlink")
	}

	err := os.Link(self, path)
	if err == nil {
		return formHardlink, nil
	}

	// A hardlink cannot span volumes: it is a second directory entry for one
	// file, and a file lives on one volume. Said plainly, with the reason,
	// because the fix is to move the binary and nothing else on screen would
	// suggest that.
	if note, ok := crossDeviceNote(self, path); ok {
		_, _ = fmt.Fprintf(out, "\n%s\n", note)
		_, _ = fmt.Fprintf(out,
			"That leaves a full copy: %d MB, and it will not follow upgrades.\n"+
				"  avoid it by putting remote-docker on the same volume as %s\n",
			sizeMB(self), filepath.Dir(path))
	} else {
		_, _ = fmt.Fprintf(out, "\nlinking %s failed: %v\nThat leaves a full copy of the binary.\n",
			path, err)
	}

	if !allowCopy && !confirm(out, in, "Copy the binary anyway?") {
		return "", errors.New("nothing was installed; re-run with --copy to accept the copy without being asked")
	}
	if err := copyFile(self, path); err != nil {
		return "", err
	}
	return formCopy, nil
}

// confirm asks a yes/no question, and treats anything that is not a clear yes
// as a no.
//
// A closed or absent input means no. This runs in scripts and in CI, where
// there is nobody to ask, and the thing being asked about duplicates 45MB
// and silently goes stale, so proceeding unasked is the wrong default.
func confirm(out io.Writer, in io.Reader, question string) bool {
	if in == nil {
		return false
	}
	_, _ = fmt.Fprintf(out, "%s [y/N] ", question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		_, _ = fmt.Fprintln(out)
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return fmt.Errorf("reading %s: %w", from, err)
	}
	defer func() { _ = src.Close() }()

	// Written to a temporary name and renamed, so an interrupted copy cannot
	// leave a truncated binary sitting on PATH under the name docker.
	tmp := to + ".partial"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copying to %s: %w", tmp, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finishing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, to); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("putting %s in place: %w", to, err)
	}
	return nil
}

func sizeMB(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size() / (1 << 20)
}

// uninstallShim removes the shim, and the PATH entry if we added it.
func uninstallShim(out io.Writer, self string) error {
	in, err := inspectShim(self)
	if err != nil {
		return err
	}
	if !in.exists {
		_, _ = fmt.Fprintf(out, "nothing installed at %s\n", in.path)
		return nil
	}
	if !in.ours {
		return fmt.Errorf(
			"%s is not a remote-docker binary, so it was left alone", in.path)
	}
	dir := filepath.Dir(in.path)
	m, _ := readMarker(dir)

	if err := os.Remove(in.path); err != nil {
		return fmt.Errorf("removing %s: %w", in.path, err)
	}
	_, _ = fmt.Fprintf(out, "removed %s\n", in.path)
	_ = os.Remove(markerPath(dir))

	// Only if we added it. Removing an entry somebody put there themselves
	// would be a worse failure than leaving one of ours behind, and the marker
	// is the only thing that knows which this is.
	if !m.AddedPATH {
		return nil
	}
	return removePATH(out, dir)
}

// reportShim is the `shim status` body, and the same lines `status` shows.
func reportShim(out io.Writer, self string) {
	in, err := inspectShim(self)
	if err != nil {
		row(out, "docker shim", err.Error())
		return
	}
	switch {
	case !in.exists:
		rowf(out, "docker shim", "not installed; `remote-docker shim install` puts `docker` on PATH")
	case !in.ours:
		rowf(out, "docker shim", "%s, not ours, left alone", in.path)
	case in.current:
		rowf(out, "docker shim", "%s (%s)", in.path, in.form)
	default:
		rowf(out, "docker shim", "%s (%s), STALE: an older build. Run `shim install` again",
			in.path, in.form)
	}
}

func newShimCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shim",
		Short: "Install this binary on PATH under the name docker",
		Long: `Puts a "docker" on PATH that is this binary, so ordinary commands work
without a prefix:

    docker run --rm -v .:/w alpine ls /w

Nothing is downloaded. This binary already carries the Docker CLI and answers
to the name it is invoked by, so the shim is a link rather than a second
program. Renaming the binary to docker` + exeSuffixDoc() + ` does the same thing by hand.

"docker compose" works through it too: that is in here as well.`,
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}
	cmd.AddCommand(newShimInstallCommand(), newShimUninstallCommand(), newShimStatusCommand())
	return cmd
}

func exeSuffixDoc() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func newShimInstallCommand() *cobra.Command {
	var (
		noPath bool
		copyOK bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Put `docker` on PATH",
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := selfPath()
			if err != nil {
				return fmt.Errorf("finding this binary: %w", err)
			}
			// selfPath here and NOT os.Args[0], which is the opposite of
			// alias.go and for the opposite reason: this needs the real file
			// to link TO, not the name we were called by. selfPath rather than
			// os.Executable because on Termux that is the system linker, and
			// this would have put a `docker` on PATH that was the linker.
			out := cmd.OutOrStdout()

			if err := installShim(out, cmd.InOrStdin(), self, copyOK); err != nil {
				return err
			}

			dir, err := shimDir()
			if err != nil {
				return err
			}
			if noPath {
				_, _ = fmt.Fprintf(out, "PATH left alone. Add %s to it yourself.\n", dir)
				return nil
			}

			added, err := ensurePATH(out, dir)
			if err != nil {
				return err
			}
			if added {
				// Recorded so uninstall knows the entry is ours to remove.
				m, _ := readMarker(dir)
				m.AddedPATH = true
				if err := writeMarker(dir, m); err != nil {
					return fmt.Errorf("recording the PATH change: %w", err)
				}
			}

			_, _ = fmt.Fprintln(out, "\nTry `docker version`. If this shell cannot find it, open a new one.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&noPath, "no-path", false, "install the file but do not touch PATH")
	cmd.Flags().BoolVar(&copyOK, "copy", false, "accept a full copy of the binary without being asked")
	return cmd
}

func newShimUninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the `docker` this installed",
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := selfPath()
			if err != nil {
				return fmt.Errorf("finding this binary: %w", err)
			}
			return uninstallShim(cmd.OutOrStdout(), self)
		},
	}
}

func newShimStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Say where the `docker` shim is and whether it is current",
		RunE: func(cmd *cobra.Command, _ []string) error {
			self, err := selfPath()
			if err != nil {
				return fmt.Errorf("finding this binary: %w", err)
			}
			reportShim(cmd.OutOrStdout(), self)
			return nil
		},
	}
}
