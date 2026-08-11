package machine

// The WSL backend's decisions, tested on a machine with no WSL.
//
// The output samples are real shapes from `wsl -l -v`: UTF-16 with a BOM, an
// asterisk column for the default distribution, and a header row in whatever
// language the Windows speaks.

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// utf16le encodes a string the way wsl.exe writes one, so the tests exercise
// the decoder rather than a convenient fiction.
func utf16le(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2+2)
	out = append(out, 0xff, 0xfe) // the BOM wsl.exe writes
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func TestDecodeWSLOutput(t *testing.T) {
	// The failure this exists for: read as bytes, UTF-16 looks like text with
	// a NUL between every character, and every Contains against it fails for a
	// reason nothing on screen explains.
	if got := decodeWSLOutput(utf16le("Running\n")); got != "Running\n" {
		t.Errorf("UTF-16 decoded to %q", got)
	}
	// Older builds write plain ASCII, and must still work.
	if got := decodeWSLOutput([]byte("Running\n")); got != "Running\n" {
		t.Errorf("ASCII decoded to %q", got)
	}
	if got := decodeWSLOutput(nil); got != "" {
		t.Errorf("empty output decoded to %q", got)
	}
}

func TestParseWSLList(t *testing.T) {
	listing := utf16le(
		"  NAME                   STATE           VERSION\n" +
			"* Ubuntu                 Running         2\n" +
			"  rd-dev                 Stopped         2\n" +
			"  docker-desktop         Running         2\n" +
			"  legacy                 Stopped         1\n")

	got := parseWSLList(listing)
	if len(got) != 4 {
		t.Fatalf("parsed %d distributions, want 4: %+v", len(got), got)
	}

	// The asterisk is a COLUMN. Read as part of the name, a machine that
	// happens to be the user's default is called "*rd-dev" and never found.
	if got[0].Name != "Ubuntu" {
		t.Errorf("the default distribution is named %q, want Ubuntu", got[0].Name)
	}
	if got[1].Name != "rd-dev" || got[1].State != "Stopped" || got[1].Version != 2 {
		t.Errorf("rd-dev parsed as %+v", got[1])
	}
	if got[3].Version != 1 {
		t.Errorf("a version 1 distribution parsed as %+v", got[3])
	}
}

// The header is skipped by shape rather than by its words, so a Windows in
// another language does not produce a distribution called "NAME".
func TestParseWSLListSkipsAHeaderInAnyLanguage(t *testing.T) {
	german := utf16le(
		"  NAME                   STATUS          VERSION\n" +
			"* rd-dev                 Wird ausgeführt 2\n")

	got := parseWSLList(german)
	if len(got) != 1 {
		t.Fatalf("parsed %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Name != "rd-dev" || got[0].Version != 2 {
		t.Errorf("parsed %+v", got[0])
	}
}

func TestObserveWSL(t *testing.T) {
	distros := []WSLDistribution{
		{Name: "Ubuntu", State: "Running", Version: 2},
		{Name: "rd-dev", State: "Stopped", Version: 2},
		{Name: "rd-old", State: "Running", Version: 1},
	}

	for _, tc := range []struct {
		name  string
		which string
		want  State
	}{
		{"a stopped machine", "rd-dev", Stopped},
		{"one that is not there", "rd-missing", Absent},

		// Version 1 cannot run the agent: no real kernel, so no dockerd and no
		// NFS mount. Reported as absent so the caller creates rather than
		// starting something that will fail obscurely.
		{"a version 1 distribution", "rd-old", Absent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := observeWSL(distros, tc.which, "abc"); got.State != tc.want {
				t.Errorf("observeWSL(%s) = %v, want %v", tc.which, got.State, tc.want)
			}
		})
	}

	// Case, because Windows is.
	if got := observeWSL(distros, "RD-DEV", "abc"); got.State != Stopped {
		t.Errorf("a differently cased name was not found: %v", got.State)
	}
	if got := observeWSL(distros, "rd-dev", "abc"); got.Generation != "abc" {
		t.Errorf("the generation was not carried through: %q", got.Generation)
	}
}

func TestWSLName(t *testing.T) {
	// A distribution list is the user's own namespace, holding their Ubuntu
	// and whatever else. Taking a bare name there is the mistake ADR 0025
	// records for unix accounts.
	if got := WSLName("dev"); got != "rd-dev" {
		t.Errorf("WSLName(dev) = %q", got)
	}
}

func TestWSLArgs(t *testing.T) {
	imp := wslImportArgs("rd-dev", `C:\wsl\rd-dev`, `C:\wsl\rootfs.tar`, 2)
	want := []string{"--import", "rd-dev", `C:\wsl\rd-dev`, `C:\wsl\rootfs.tar`, "--version", "2"}
	for i := range want {
		if imp[i] != want[i] {
			t.Fatalf("import args = %v, want %v", imp, want)
		}
	}

	run := wslRunArgs("rd-dev", "sh", "-c", "echo hi")
	// --user root, because an imported rootfs's default user need not stay
	// root; --cd /, because WSL otherwise starts in the Windows working
	// directory translated into /mnt, which may not exist.
	joined := ""
	for _, a := range run {
		joined += a + " "
	}
	for _, want := range []string{"--user root", "--cd /", "-d rd-dev", "-- sh"} {
		if !contains(joined, want) {
			t.Errorf("run args %q are missing %q", joined, want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Quoting, which was wrong once already: an escaping mistake here turns a file
// write into whatever the content happens to say.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"abc123", `'abc123'`},
		{"", `''`},
		// The reason this exists: a newline in the content must not end the
		// command.
		{"[boot]\ncommand=x", "'[boot]\ncommand=x'"},
		// The only escape sh understands inside single quotes: end the quote,
		// an escaped quote, start again.
		{"it's", `'it'\''s'`},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestWSLWriteArgs(t *testing.T) {
	args := wslWriteArgs("rd-dev", "/etc/wsl.conf", "[boot]\nsystemd=false\n")

	// The command must reach sh as ONE argument, or the shell sees the config's
	// own newlines as command separators.
	last := args[len(args)-1]
	if !contains(last, "/etc/wsl.conf") || !contains(last, "systemd=false") {
		t.Fatalf("the write command lost its content or its path: %q", last)
	}
	if !contains(last, "printf") {
		t.Errorf("expected printf, got %q", last)
	}
}

func TestWSLReadGenerationArgs(t *testing.T) {
	args := wslReadGenerationArgs("rd-dev")
	joined := strings.Join(args, " ")
	if !contains(joined, "cat "+generationFile) {
		t.Errorf("the generation is not read from %s: %q", generationFile, joined)
	}
	// Inside the distribution, so a machine exported and re-imported by hand
	// carries its own answer rather than trusting a file next to the config.
	if !strings.HasPrefix(generationFile, "/") {
		t.Errorf("the generation file is not an absolute path inside the machine: %q", generationFile)
	}
}

// The agent binds every interface inside the machine, not its loopback.
//
// Windows reaches the machine either through WSL2's localhost relay or at the
// machine's own address, and a service on the machine's loopback can only ever
// be reached by the first of those.
func TestWSLConfBindsBeyondLoopback(t *testing.T) {
	conf := wslConf(Spec{Name: "dev", Port: 2222})

	if strings.Contains(conf, "127.0.0.1:2222") {
		t.Error("the agent is bound to the machine's loopback, which Windows cannot reach")
	}
	if !strings.Contains(conf, "--addr :2222") {
		t.Errorf("expected --addr :2222 in:\n%s", conf)
	}
	// The boot command is what starts the agent after a reboot, with nothing
	// on the Windows side supervising anything.
	if !strings.Contains(conf, "[boot]") || !strings.Contains(conf, "command=") {
		t.Errorf("no boot command in:\n%s", conf)
	}
}

// The image's environment has to be restored by hand.
//
// `docker export` writes a filesystem; ENV and PATH live in the image config
// beside the layers and are not in the tarball. It fails a long way from the
// cause: dockerd-entrypoint.sh is not on a PATH without /usr/local/bin, the
// agent restarts it forever and blocks its own listener, and Windows sees a
// refused connection.
func TestWSLConfCarriesTheImageEnvironment(t *testing.T) {
	conf := wslConf(Spec{Name: "dev", Port: 2222})

	if !strings.Contains(conf, "/usr/local/bin") {
		t.Errorf("PATH has no /usr/local/bin, where the agent and dockerd's entrypoint live:\n%s", conf)
	}
	// Empty, not absent: this is how the image turns dind's TLS off, and unset
	// means dind generates certificates and listens on another port instead.
	if !strings.Contains(conf, "DOCKER_TLS_CERTDIR= ") {
		t.Errorf("DOCKER_TLS_CERTDIR is not set to empty:\n%s", conf)
	}
}

// A machine runs one daemon, not one per account.
//
// It has exactly one account -- the person whose computer it is -- so a daemon
// each separates nobody from anybody and costs a nested dind container, a
// second graph store and a duplicated layer cache.
func TestWSLConfUsesTheSharedDaemon(t *testing.T) {
	if conf := wslConf(Spec{Name: "dev", Port: 2222}); !strings.Contains(conf, "WORKSPACE_PER_USER_DIND=false") {
		t.Errorf("a machine asks for a daemon per account:\n%s", conf)
	}
}
