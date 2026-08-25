package main

import (
	"reflect"
	"strings"
	"testing"
)

// The environment Git Bash passes to a native child, and the paths it maps onto.
// Every string below was measured on 2026-08-25 by printing the argv a native
// Windows program actually receives.
const (
	testRoot = `C:\Program Files\Git`
	testTemp = `C:\Users\pierr\AppData\Local\Temp`
)

func testEnv(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func testMSYS() msys {
	return msysFrom(testEnv(map[string]string{
		"EXEPATH": testRoot + `\bin`,
		"MSYSTEM": "MINGW64",
		"TEMP":    testTemp,
	}))
}

func TestMSYSFromEnv(t *testing.T) {
	m := testMSYS()
	if !m.known() || m.root != testRoot {
		t.Fatalf("root = %q, want %q", m.root, testRoot)
	}

	// SHELL is two levels down from the root, and is the fallback.
	viaShell := msysFrom(testEnv(map[string]string{
		"SHELL":   testRoot + `\bin\bash.exe`,
		"MSYSTEM": "MINGW64",
	}))
	if viaShell.root != testRoot {
		t.Errorf("via SHELL: root = %q, want %q", viaShell.root, testRoot)
	}

	// No Git Bash means no repair at all, which is every other shell.
	for _, env := range []map[string]string{
		{},
		{"EXEPATH": testRoot + `\bin`}, // MSYSTEM is what says this is MSYS
		{"MSYSTEM": "MINGW64"},         // and EXEPATH is what says where
	} {
		if m := msysFrom(testEnv(env)); m.known() {
			t.Errorf("msysFrom(%v) claims to know Git Bash", env)
		}
	}
}

// The table is the measurement: each mangled value must come back as the
// specification that was typed at the prompt. The source keeps the Windows
// spelling MSYS correctly gave it; only the target is restored.
func TestUnmangleBind(t *testing.T) {
	m := testMSYS()
	for _, c := range []struct {
		name    string
		mangled string
		want    string
	}{
		{"a directory and a target",
			`C:\Users\pierr\x;C:\Program Files\Git\app`,
			`C:\Users\pierr\x:/app`},
		{"read-only survives",
			`C:\Users\pierr\x;C:\Program Files\Git\app;ro`,
			`C:\Users\pierr\x:/app:ro`},
		{"a single-letter target became a drive",
			`C:\Program Files\Git\etc\hostname;X:\`,
			`C:\Program Files\Git\etc\hostname:/x`},
		{"a relative source",
			`.\rel;C:\Program Files\Git\app`,
			`.\rel:/app`},
		{"a target under TEMP, which is not under the root",
			`C:\data;C:\Users\pierr\AppData\Local\Temp\cache`,
			`C:\data:/tmp/cache`},
		{"a nested target",
			`C:\data;C:\Program Files\Git\etc\nginx\nginx.conf`,
			`C:\data:/etc/nginx/nginx.conf`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _, ok := m.unmangleBind(c.mangled)
			if !ok {
				t.Fatalf("unmangleBind(%q) reported nothing to repair", c.mangled)
			}
			if got != c.want {
				t.Errorf("unmangleBind(%q) =\n%q\nwant\n%q", c.mangled, got, c.want)
			}
		})
	}
}

// Anything that is not this conversion is returned untouched, because the
// trigger is two conditions and not one.
func TestUnmangleBindLeavesEverythingElse(t *testing.T) {
	m := testMSYS()
	for _, value := range []string{
		`x_named:/app`,             // a named volume, never converted
		`C:\Users\pierr\x:/app`,    // typed in Windows form, never converted
		`C:\Users\pierr\x:/app:ro`, //
		`/home/me/project:/app`,    // a POSIX pair from a real shell
		`C:\my;dir:/app`,           // a semicolon in a NAME, which NTFS allows
		`a;b;c;d`,                  // too many fields to be a bind
	} {
		if got, _, ok := m.unmangleBind(value); ok {
			t.Errorf("unmangleBind(%q) repaired it to %q", value, got)
		}
	}
}

// /bin and /usr/bin are one directory in Git Bash, so that reversal is a guess
// and says so. Nothing else is ambiguous -- /lib and /usr/lib are distinct.
func TestUnmangleBindWarnsOnlyWhenAmbiguous(t *testing.T) {
	m := testMSYS()

	_, note, ok := m.unmangleBind(`C:\data;C:\Program Files\Git\usr\bin`)
	if !ok {
		t.Fatal("the /usr/bin case was not repaired")
	}
	if !strings.Contains(note, "/usr/bin") || !strings.Contains(note, "/bin") {
		t.Errorf("note = %q, want both readings named", note)
	}

	for _, mangled := range []string{
		`C:\data;C:\Program Files\Git\app`,
		`C:\data;C:\Program Files\Git\lib\modules`,
		`C:\data;C:\Program Files\Git\usr\lib`,
	} {
		if _, note, ok := m.unmangleBind(mangled); ok && note != "" {
			t.Errorf("unmangleBind(%q) warned about an exact reversal: %s", mangled, note)
		}
	}
}

// A target this program cannot invert is left alone and reported, because a
// silent wrong guess is worse than an error the user can act on.
func TestUnmangleBindReportsATargetItCannotRestore(t *testing.T) {
	_, note, ok := testMSYS().unmangleBind(`C:\data;D:\somewhere\else`)
	if ok {
		t.Error("a target outside every known mapping was rewritten anyway")
	}
	if !strings.Contains(note, "MSYS_NO_PATHCONV") {
		t.Errorf("note = %q, want the escape named", note)
	}
}

func TestRepairArgs(t *testing.T) {
	m := testMSYS()
	const mangled = `C:\Users\pierr\x;C:\Program Files\Git\app;ro`
	const want = `C:\Users\pierr\x:/app:ro`

	got, notes := m.repairArgs([]string{
		"run", "--rm",
		"-v", mangled,
		"--volume", mangled,
		"-v=" + mangled,
		"--volume=" + mangled,
		"-w", `C:/Program Files/Git/src`, // not a -v, and not ours to touch
		"alpine",
	})
	expect := []string{
		"run", "--rm",
		"-v", want,
		"--volume", want,
		"-v=" + want,
		"--volume=" + want,
		"-w", `C:/Program Files/Git/src`,
		"alpine",
	}
	if !reflect.DeepEqual(got, expect) {
		t.Errorf("repairArgs =\n%q\nwant\n%q", got, expect)
	}
	if len(notes) != 0 {
		t.Errorf("an exact repair produced notes: %v", notes)
	}
}

// A trailing -v with nothing after it must not panic, which is the shape a
// half-typed command line has.
func TestRepairArgsWithNothingAfterTheFlag(t *testing.T) {
	got, _ := testMSYS().repairArgs([]string{"run", "-v"})
	if !reflect.DeepEqual(got, []string{"run", "-v"}) {
		t.Errorf("repairArgs = %q", got)
	}
}

func TestRepairArgsDoesNothingOutsideGitBash(t *testing.T) {
	args := []string{"-v", `C:\Users\pierr\x;C:\Program Files\Git\app`}
	got, notes := msys{}.repairArgs(args)
	if !reflect.DeepEqual(got, args) || notes != nil {
		t.Errorf("repairArgs without Git Bash = %q, %v", got, notes)
	}
}
