package machine

// The Hyper-V backend's decisions, tested on a machine with no Hyper-V.
//
// This is the only coverage this backend has or can have. GitHub's runners do
// not offer Hyper-V and nobody working on this project has it, so what is not
// pinned here ships unexecuted -- which is why so much of the backend is a
// function of a string.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseVMState(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want State
	}{
		{"Running", Running},
		{"running", Running},
		{"Off", Stopped},
		// Saved and Paused are startable, and Start-VM resumes them correctly.
		// Reporting them as anything else would send the caller into create,
		// which discards the machine.
		{"Saved", Stopped},
		{"Paused", Stopped},
		{"", Absent},
	} {
		if got := parseVMState(tc.in); got != tc.want {
			t.Errorf("parseVMState(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseVMAddress(t *testing.T) {
	// The real shape: Get-VMNetworkAdapter reports every address the guest told
	// Hyper-V about, IPv6 and link-local included.
	real := "fe80::215:5dff:fe00:1,172.19.4.7,2001:db8::1"
	if got := parseVMAddress(real); got != "172.19.4.7" {
		t.Errorf("parseVMAddress = %q, want 172.19.4.7", got)
	}

	// Link-local means DHCP has not finished. That is a machine which is up and
	// not ready, and connecting to it would fail in a way that names the wrong
	// thing -- so it reports no address and the caller keeps waiting.
	if got := parseVMAddress("169.254.12.9,fe80::1"); got != "" {
		t.Errorf("a link-local address was offered as reachable: %q", got)
	}
	if got := parseVMAddress(""); got != "" {
		t.Errorf("no addresses parsed as %q", got)
	}
}

func TestNotesRoundTrip(t *testing.T) {
	notes := hyperVNotes{Generation: "abc123", Key: "deadbeef"}
	if got := decodeNotes(encodeNotes(notes)); got != notes {
		t.Errorf("notes round-tripped to %+v", got)
	}

	// A Notes field somebody typed into by hand is not a generation mismatch.
	// Treating it as one would recreate their machine and take its containers,
	// which is the expensive direction to be wrong in.
	if got := decodeNotes("my dev box, do not delete"); got.Generation != "" {
		t.Errorf("prose parsed as a generation: %q", got.Generation)
	}
}

func TestHyperVEnrolment(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3Nza deadbeef"

	// The machine was built with this key: nothing to do, which is what enrol
	// means on a backend that cannot write one.
	if err := hyperVEnrolment(hyperVNotes{Key: keyFingerprint(key)}, key); err != nil {
		t.Errorf("the key it was built with was refused: %v", err)
	}
	// Trailing whitespace is not a different key.
	if err := hyperVEnrolment(hyperVNotes{Key: keyFingerprint(key)}, key+"\n"); err != nil {
		t.Errorf("a newline made it a different key: %v", err)
	}
	// A machine from before this was recorded is assumed to match, because
	// refusing every older machine over a fingerprint nobody wrote down is
	// worse than the connection error a wrong key gives.
	if err := hyperVEnrolment(hyperVNotes{}, key); err != nil {
		t.Errorf("a machine with no recorded key was refused: %v", err)
	}

	// A different key is reported rather than accepted. Silently accepting one
	// that cannot work is the failure that succeeds.
	err := hyperVEnrolment(hyperVNotes{Key: keyFingerprint("ssh-ed25519 AAAAC3Nza somebodyelse")}, key)
	if err == nil {
		t.Fatal("a key the machine was not built with was accepted")
	}
	if !strings.Contains(err.Error(), "rebuild") {
		t.Errorf("the error does not say what to do: %v", err)
	}

	// The fingerprint is not the key. What is stored sits in a VM's Notes,
	// which is not a place to put anything secret.
	if strings.Contains(keyFingerprint(key), "deadbeef") {
		t.Error("the fingerprint contains the key")
	}
}

func TestIgnition(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3Nza+slash/and+plus dev@host"

	raw, err := ignition(Spec{Name: "dev", Account: "dev", Port: 2222, Image: "example.com/ws:1"}, key)
	if err != nil {
		t.Fatalf("ignition: %v", err)
	}

	// It has to be a document Ignition can read, which means it has to parse.
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the config is not JSON: %v\n%s", err, raw)
	}
	if doc["ignition"] == nil || doc["systemd"] == nil || doc["storage"] == nil {
		t.Errorf("the config is missing a section:\n%s", raw)
	}

	// The key must survive encoding intact. `+` is the trap: url.QueryEscape
	// turns it into a space, and a key with a space where a + was is a
	// different key that will never authenticate.
	if !strings.Contains(raw, urlEncode(key+"\n")) {
		t.Errorf("the key is not in the config as written:\n%s", raw)
	}
	if strings.Contains(urlEncode("a+b"), " ") {
		t.Error("urlEncode turned a + into a space, which silently changes the key")
	}
	if urlEncode("a+b") != "a%2Bb" {
		t.Errorf("urlEncode(a+b) = %q", urlEncode("a+b"))
	}
}

func TestHyperVUnit(t *testing.T) {
	unit := hyperVUnit(Spec{Port: 2222, Image: "example.com/ws:1"})

	if !strings.Contains(unit, "example.com/ws:1") {
		t.Errorf("the unit does not run the image it was given:\n%s", unit)
	}
	if !strings.Contains(unit, "--addr :2222") {
		t.Errorf("the unit does not serve on the machine's port:\n%s", unit)
	}
	// No --rm. The container holds the machine's docker state, and a restart
	// that discarded it would lose every image the user built.
	if strings.Contains(unit, "--rm") {
		t.Errorf("the workspace container is disposable, which loses its images:\n%s", unit)
	}
	if !strings.Contains(unit, "Restart=always") {
		t.Errorf("nothing restarts the workspace:\n%s", unit)
	}
	// A machine has one account, so the shared daemon is right here for the
	// same reason it is right on WSL.
	if !strings.Contains(unit, "WORKSPACE_PER_USER_DIND=false") {
		t.Errorf("a machine asks for a daemon per account:\n%s", unit)
	}

	// Without an image it runs the published one rather than nothing.
	if !strings.Contains(hyperVUnit(Spec{Port: 22}), DefaultImage) {
		t.Error("a spec with no image produces a unit that runs nothing")
	}
}

func TestPSQuote(t *testing.T) {
	// Doubling is the only escape inside a PowerShell single-quoted literal,
	// and using one is what keeps a $ in a machine's notes from being expanded.
	for _, tc := range []struct{ in, want string }{
		{"rd-dev", "'rd-dev'"},
		{"it's", "'it''s'"},
		{`$env:PATH`, `'$env:PATH'`},
		{"", "''"},
	} {
		if got := psQuote(tc.in); got != tc.want {
			t.Errorf("psQuote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestPSCommands(t *testing.T) {
	spec := Spec{Name: "dev", Backend: "hyperv", Port: 2222, CPUs: 4, MemoryMB: 4096}

	create := psNewVM("rd-dev", `C:\m\disk.vhdx`, `C:\m`, spec)
	for _, want := range []string{
		"New-VM -Name 'rd-dev'",
		// Generation 2 is the UEFI one, which is what Flatcar's image expects.
		"-Generation 2",
		"-SwitchName 'Default Switch'",
		// Flatcar's image is not signed by anything Hyper-V ships, so a machine
		// with secure boot on does not boot at all.
		"Set-VMFirmware -VMName 'rd-dev' -EnableSecureBoot Off",
		"Set-VMProcessor -VMName 'rd-dev' -Count 4",
		"Set-VMMemory -VMName 'rd-dev' -StartupBytes 4096MB",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("the create command is missing %q:\n%s", want, create)
		}
	}

	// Zero means the platform's default, which is a better number than one
	// invented here -- so nothing is set at all.
	bare := psNewVM("rd-dev", "d.vhdx", "d", Spec{Name: "dev"})
	if strings.Contains(bare, "Set-VMProcessor") || strings.Contains(bare, "Set-VMMemory") {
		t.Errorf("an unset cpu or memory was turned into a number:\n%s", bare)
	}

	// Destroy takes the disk with it. Remove-VM alone leaves it, which quietly
	// keeps gigabytes per machine somebody believes they removed.
	remove := psRemoveVM("rd-dev", `C:\m`)
	if !strings.Contains(remove, "Remove-Item") || !strings.Contains(remove, `'C:\m'`) {
		t.Errorf("destroy leaves the disk behind:\n%s", remove)
	}
	if !strings.Contains(remove, "Remove-VM -Name 'rd-dev' -Force") {
		t.Errorf("destroy does not remove the machine:\n%s", remove)
	}

	// State and notes in one call, so they cannot disagree about a machine that
	// changed between two.
	get := psGetVM("rd-dev")
	if !strings.Contains(get, "$vm.State") || !strings.Contains(get, "$vm.Notes") {
		t.Errorf("state and notes are not read together:\n%s", get)
	}
	if !strings.Contains(psAddress("rd-dev"), "Get-VMNetworkAdapter -VMName 'rd-dev'") {
		t.Errorf("the address is not read from the machine's adapter: %s", psAddress("rd-dev"))
	}

	// The key fingerprint is recorded only after the machine exists, so one
	// that failed halfway is never marked as built with a key it never got.
	notes := psSetNotes("rd-dev", hyperVNotes{Generation: "g", Key: "k"})
	if !strings.Contains(notes, "Set-VM -Name 'rd-dev' -Notes") || !strings.Contains(notes, `"key":"k"`) {
		t.Errorf("the notes command does not record what the machine was built with: %s", notes)
	}
}
