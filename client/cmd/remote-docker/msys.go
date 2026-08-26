package main

import "strings"

// Git Bash rewrites `-v` before this program starts (ADR 0040):
//
//	-v /c/Users/you/x:/app:ro  ->  C:\Users\you\x;C:\Program Files\Git\app;ro
//
// MSYS maps the SOURCE correctly, with a mount table this program does not
// have, and cannot map the target, because nothing tells it that `-v` has two
// halves meaning different things. Only the target is restored here.

// msys is what a Git Bash parent tells us about itself.
type msys struct {
	root string // where POSIX paths are mapped: C:\Program Files\Git
	temp string // where /tmp lands, which is NOT under root
}

// msysFrom reads the environment Git Bash passes down. A zero msys means the
// parent was not Git Bash, and nothing is repaired.
func msysFrom(getenv func(string) string) msys {
	root := ""
	switch {
	case getenv("EXEPATH") != "":
		// EXEPATH is the bin directory: C:\Program Files\Git\bin.
		root = parent(getenv("EXEPATH"))
	case strings.Contains(strings.ToLower(getenv("SHELL")), "bash.exe"):
		// SHELL is the bash binary, two levels down from the root.
		root = parent(parent(getenv("SHELL")))
	}
	if root == "" || getenv("MSYSTEM") == "" {
		return msys{}
	}
	return msys{root: root, temp: slashed(getenv("TEMP"))}
}

// slashed normalises a Windows path for comparison.
//
// NOT path/filepath, which follows the HOST's rules: on Linux a backslash is an
// ordinary character, so filepath.Dir of a Windows path is "." and every
// comparison here silently stops working. CI caught it.
func slashed(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// parent is the directory holding p, in slash form.
func parent(p string) string {
	s := strings.TrimSuffix(slashed(p), "/")
	if i := strings.LastIndex(s, "/"); i > 0 {
		return s[:i]
	}
	return ""
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
			out[i+1], notes = m.repair(out[i+1], notes)
			i++
		case strings.HasPrefix(arg, "-v="), strings.HasPrefix(arg, "--volume="):
			flag, value, _ := strings.Cut(arg, "=")
			value, notes = m.repair(value, notes)
			out[i] = flag + "=" + value
		}
	}
	return out, notes
}

// repair is one value and whatever it has to say, so the two spellings of the
// flag do not each spell out the same three steps.
func (m msys) repair(value string, notes []string) (string, []string) {
	fixed, note, ok := m.unmangleBind(value)
	if note != "" {
		notes = append(notes, note)
	}
	if !ok {
		return value, notes
	}
	return fixed, notes
}

// unmangleBind restores one bind specification.
//
// Two conditions trigger it, never one: a `;`, which a real bind never has, AND
// a target this converts back. `;` alone proves nothing, since NTFS permits it
// in a file name.
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
	field = slashed(field)

	// A single-letter path is mapped to a drive: /x becomes X:\.
	if drive, rest, found := strings.Cut(field, ":"); found && len(drive) == 1 {
		if strings.Trim(rest, "/") == "" {
			return "/" + strings.ToLower(drive), ""
		}
	}

	if restored := m.posixSource(field); restored != "" {
		// Git Bash maps /bin and /usr/bin onto one directory, so this one
		// reversal cannot be exact. Measured: /lib and /usr/lib do NOT collide.
		if restored == "/usr/bin" || strings.HasPrefix(restored, "/usr/bin/") {
			return restored, "read the target as " + restored + " (it may have been " +
				strings.Replace(restored, "/usr", "", 1) + ")"
		}
		return restored, ""
	}

	// Cannot be inverted. Saying so beats guessing.
	if looksWindows(field) {
		return "", "cannot restore the target " + field +
			"; run with MSYS_NO_PATHCONV=1 or write the target as //" + strings.TrimPrefix(field, "/")
	}
	return "", ""
}

// posixSource reports the POSIX path a converted path may have been, and "" when
// it is not one MSYS could have produced.
//
// A candidate, never a correction: `C:\Program Files\Git\etc` is what MSYS makes
// of BOTH `/etc` and `/c/Program Files/Git/etc`. Only the caller can break that
// tie -- see rewrite.ownedByDaemon, which takes it only when the workspace
// declares the path and this machine does not have it (ADR 0041).
func (m msys) posixSource(p string) string {
	p = slashed(p)
	switch {
	case same(p, m.root):
		return "/"
	case same(p, m.temp):
		return "/tmp"
	}
	if rest, ok := under(p, m.root); ok {
		return "/" + rest
	}
	if rest, ok := under(p, m.temp); ok {
		return "/tmp/" + rest
	}
	return ""
}

// under reports whether p is inside base, and what remains below it.
func under(p, base string) (string, bool) {
	if base == "" {
		return "", false
	}
	prefix := strings.TrimSuffix(base, "/") + "/"
	if len(p) <= len(prefix) || !strings.EqualFold(p[:len(prefix)], prefix) {
		return "", false
	}
	return p[len(prefix):], true
}

func same(p, base string) bool {
	return base != "" && strings.EqualFold(strings.TrimSuffix(p, "/"),
		strings.TrimSuffix(base, "/"))
}

// looksWindows reports whether a path is drive-rooted, which a container path
// never is.
func looksWindows(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}
