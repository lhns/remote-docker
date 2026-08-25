package main

import (
	"path/filepath"
	"strings"
)

// Git Bash rewrites arguments before this program starts.
//
// MSYS converts POSIX-looking arguments to Windows form in the PARENT, while
// building the command line for a native Windows child, so both argv and
// GetCommandLineW hold only the converted text. Measured 2026-08-25:
//
//	-v /c/Users/you/x:/app:ro  ->  C:\Users\you\x;C:\Program Files\Git\app;ro
//	-v /etc/hostname:/x        ->  C:\Program Files\Git\etc\hostname;X:\
//	-w /src                    ->  C:/Program Files/Git/src
//	--mount type=bind,source=/c/…,target=/app   ->   both sides correct
//
// It gets the SOURCE right, using a mount table this program does not have, and
// cannot get the target right, because nothing tells it that `-v` has two halves
// meaning different things. So only the target is restored here (ADR 0040).
//
// The callee cannot opt out of the conversion: it depends on whether the child
// links msys-2.0.dll, and MSYS_NO_PATHCONV is read from the environment by the
// caller. Undoing it is the only move available to us.

// msys is what a Git Bash parent tells us about itself.
type msys struct {
	// root is the installation directory, "C:\Program Files\Git", which POSIX
	// paths are mapped under.
	root string

	// temp is where /tmp lands, which is NOT under root: it follows the
	// Windows TEMP variable.
	temp string
}

// msysFrom reads the environment Git Bash passes down. A zero msys means the
// parent was not Git Bash, and nothing is repaired.
func msysFrom(getenv func(string) string) msys {
	root := ""
	switch {
	case getenv("EXEPATH") != "":
		// EXEPATH is the bin directory: C:\Program Files\Git\bin.
		root = filepath.Dir(getenv("EXEPATH"))
	case strings.Contains(strings.ToLower(getenv("SHELL")), "bash.exe"):
		// SHELL is the bash binary, two levels down from the root.
		root = filepath.Dir(filepath.Dir(getenv("SHELL")))
	}
	if root == "" || getenv("MSYSTEM") == "" {
		return msys{}
	}
	return msys{root: root, temp: getenv("TEMP")}
}

func (m msys) known() bool { return m.root != "" }

// repairArgs restores the `-v` values Git Bash rewrote, and returns one note per
// repair that a reader needs to see. Everything else is returned untouched.
func (m msys) repairArgs(args []string) ([]string, []string) {
	if !m.known() {
		return args, nil
	}

	out := make([]string, len(args))
	copy(out, args)
	var notes []string

	for i := 0; i < len(out); i++ {
		switch arg := out[i]; {
		case arg == "-v" || arg == "--volume":
			if i+1 >= len(out) {
				continue
			}
			fixed, note, ok := m.unmangleBind(out[i+1])
			if ok {
				out[i+1] = fixed
			}
			notes = appendNote(notes, note)
			i++
		case strings.HasPrefix(arg, "-v="), strings.HasPrefix(arg, "--volume="):
			flag, value, _ := strings.Cut(arg, "=")
			fixed, note, ok := m.unmangleBind(value)
			if ok {
				out[i] = flag + "=" + fixed
			}
			notes = appendNote(notes, note)
		}
	}
	return out, notes
}

func appendNote(notes []string, note string) []string {
	if note == "" {
		return notes
	}
	return append(notes, note)
}

// unmangleBind restores one bind specification, reporting whether it was
// mangled at all and what a reader should be told.
//
// The trigger is deliberately two conditions. A `;` alone proves nothing: NTFS
// permits it in a file name. A `;` AND a Windows-shaped target is a shape a real
// bind specification cannot have, because the target is a path in a Linux
// container.
func (m msys) unmangleBind(value string) (repaired, note string, ok bool) {
	if !strings.Contains(value, ";") {
		return "", "", false
	}
	fields := strings.Split(value, ";")
	if len(fields) < 2 || len(fields) > 3 {
		return "", "", false
	}

	target, note := m.unmangleTarget(fields[1])
	if target == "" {
		// Nothing to repair, but there may still be something to say: a target
		// this program cannot invert is worth reporting rather than dropping.
		return "", note, false
	}
	fields[1] = target
	return strings.Join(fields, ":"), note, true
}

// unmangleTarget maps one converted container path back. An empty result means
// this was not a converted path, so the argument is left alone.
func (m msys) unmangleTarget(field string) (target, note string) {
	slashed := filepath.ToSlash(field)

	// A single-letter path is mapped to a drive: /x becomes X:\.
	if drive, rest, found := strings.Cut(slashed, ":"); found && len(drive) == 1 {
		if strings.Trim(rest, "/") == "" {
			return "/" + strings.ToLower(drive), ""
		}
	}

	if rest, ok := under(slashed, m.root); ok {
		restored := "/" + rest
		// Git Bash maps /bin and /usr/bin onto one directory, so this one
		// reversal cannot be exact. Measured: /lib and /usr/lib do NOT collide.
		if restored == "/usr/bin" || strings.HasPrefix(restored, "/usr/bin/") {
			return restored, "read the target as " + restored + " (it may have been " +
				strings.Replace(restored, "/usr", "", 1) + ")"
		}
		return restored, ""
	}

	// /tmp follows the Windows TEMP variable rather than living under the root.
	if rest, ok := under(slashed, m.temp); ok {
		return "/tmp/" + rest, ""
	}
	if same(slashed, m.temp) {
		return "/tmp", ""
	}

	// A Windows path this program cannot invert. Saying so beats guessing: the
	// user needs MSYS_NO_PATHCONV=1 or a leading double slash.
	if looksWindows(slashed) {
		return "", "cannot restore the target " + field +
			"; run with MSYS_NO_PATHCONV=1 or write the target as //" + strings.TrimPrefix(slashed, "/")
	}
	return "", ""
}

// under reports whether p is inside base, and what remains below it.
func under(p, base string) (string, bool) {
	if base == "" {
		return "", false
	}
	prefix := strings.TrimSuffix(filepath.ToSlash(base), "/") + "/"
	if len(p) <= len(prefix) || !strings.EqualFold(p[:len(prefix)], prefix) {
		return "", false
	}
	return p[len(prefix):], true
}

func same(p, base string) bool {
	return base != "" && strings.EqualFold(strings.TrimSuffix(p, "/"),
		strings.TrimSuffix(filepath.ToSlash(base), "/"))
}

// looksWindows reports whether a path is drive-rooted, which a container path
// never is.
func looksWindows(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}
