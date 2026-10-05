package machine

// The Hyper-V backend's decisions, tested on a machine with no Hyper-V.
//
// This is the only automated coverage this backend has or can have. GitHub's
// runners do not offer Hyper-V, so what is not pinned here is checked only when
// somebody runs docs/testing-machines.md by hand, which is why so much of the
// backend is a function of a string.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestObserveVM(t *testing.T) {
	// A PowerShell that failed says nothing about whether the machine is
	// there. Reported as absent, `machine status` prints `absent` for a machine
	// that is running and `machine create` proceeds against a name already
	// taken, where the error names creation rather than what actually happened.
	boom := errors.New("powershell.exe: exit status 1")
	if _, err := observeVM(Absent, hyperVNotes{}, boom); !errors.Is(err, boom) {
		t.Errorf("a PowerShell failure was not reported: %v", err)
	}
	if _, err := observeVM(Running, hyperVNotes{Generation: "g"}, boom); err == nil {
		t.Error("a PowerShell failure on a running machine was swallowed")
	}

	// Absence has its own signal and needs no error: psGetVM prints nothing
	// when the VM is not there.
	got, err := observeVM(Absent, hyperVNotes{}, nil)
	if err != nil {
		t.Fatalf("a machine that is simply not there was reported as a failure: %v", err)
	}
	if got.State != Absent {
		t.Errorf("nothing printed was read as %v", got.State)
	}

	if got, err = observeVM(Running, hyperVNotes{Generation: "g"}, nil); err != nil {
		t.Fatalf("observeVM: %v", err)
	}
	if got.State != Running || got.Generation != "g" {
		t.Errorf("a running machine observed as %+v", got)
	}
}

func TestHalfCreatedMachineIsRebuildable(t *testing.T) {
	spec := Spec{Name: "dev", Backend: "hyperv", Port: 2222}

	// What New-VM leaves behind is what a create that died before psSetNotes
	// leaves behind: no key, and a generation saying so.
	notes := decodeNotes(notesArg(t, psNewVM("rd-dev", "d.vhdx", "d", spec)))
	if notes.Generation == spec.Generation() {
		t.Fatal("New-VM marks the machine as built from the spec, so a create that died halfway matches and nothing rebuilds it")
	}
	if notes.Key != "" {
		t.Errorf("New-VM records a key the machine has not been given: %q", notes.Key)
	}

	// Leaving the notes empty would not be enough: an unreadable generation is
	// read as a match, so the half-built machine would still report healthy.
	// See Observed.Generation and hyperVBuilding.
	if notes.Generation == "" {
		t.Fatal("an empty generation is read as a match, which is the fault this is meant to close")
	}

	observed, err := observeVM(Stopped, notes, nil)
	if err != nil {
		t.Fatalf("observeVM: %v", err)
	}
	if got := Plan(spec, observed); got != Recreate {
		t.Errorf("a half-created machine plans %v, so nothing offers to rebuild the one thing nothing can log into", got)
	}

	// psSetNotes is the single point at which a machine becomes built, and it
	// writes both fields.
	done := decodeNotes(notesArg(t, psSetNotes("rd-dev", hyperVNotes{Generation: spec.Generation(), Key: "k"})))
	if done.Generation != spec.Generation() || done.Key != "k" {
		t.Fatalf("the finished notes record %+v", done)
	}
	if got := Plan(spec, Observed{State: Running, Generation: done.Generation}); got != Nothing {
		t.Errorf("a finished machine plans %v", got)
	}
}

// notesArg pulls the JSON out of the `-Notes '...'` argument of a PowerShell
// command, undoing psQuote.
func notesArg(t *testing.T, script string) string {
	t.Helper()

	_, rest, ok := strings.Cut(script, "-Notes '")
	if !ok {
		t.Fatalf("no -Notes argument in: %s", script)
	}
	// psQuote doubles a single quote, and JSON produced here contains none, so
	// the first quote ends the literal.
	quoted, _, ok := strings.Cut(rest, "'")
	if !ok {
		t.Fatalf("unterminated -Notes argument in: %s", script)
	}
	return quoted
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

// The host key the client pins goes in at first boot, where the container
// hyperVUnit runs reads it; without one the agent makes its own.
func TestIgnitionCarriesTheHostKey(t *testing.T) {
	const hostKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbn+/=\n-----END OPENSSH PRIVATE KEY-----\n"
	base := Spec{Name: "dev", Account: "dev", Port: 2222, Image: "example.com/ws:1"}

	keyed := base
	keyed.HostKey = hostKey
	raw, err := ignition(keyed, "ssh-ed25519 AAAA dev@host")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Storage struct {
			Files []struct {
				Path     string `json:"path"`
				Mode     int    `json:"mode"`
				Contents struct {
					Source string `json:"source"`
				} `json:"contents"`
			} `json:"files"`
		} `json:"storage"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range doc.Storage.Files {
		if f.Path != hostKeyFile {
			continue
		}
		found = true
		if f.Contents.Source != "data:,"+urlEncode(hostKey) || f.Mode != 0o600 {
			t.Errorf("host key file = mode %o, source %q", f.Mode, f.Contents.Source)
		}
	}
	if !found {
		t.Errorf("no %s in:\n%s", hostKeyFile, raw)
	}
	if !strings.Contains(hyperVUnit(keyed), "-v /etc/workspace:/etc/workspace") {
		t.Error("the workspace container does not see the directory the host key is written to")
	}

	raw, err = ignition(base, "ssh-ed25519 AAAA dev@host")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, hostKeyFile) {
		t.Errorf("a spec with no host key wrote one:\n%s", raw)
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
	if !strings.Contains(hyperVUnit(Spec{Port: 22}), defaultImageRepo) {
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
	// keeps gigabytes per machine somebody believes they removed, and a disk
	// that cannot be deleted is reported rather than silenced.
	remove := psRemoveVM("rd-dev", `C:\m`)
	if !strings.Contains(remove, `Remove-Item -LiteralPath 'C:\m' -Recurse -Force }`) {
		t.Errorf("destroy leaves the disk behind:\n%s", remove)
	}
	if !strings.Contains(remove, "Remove-VM -VM $vm -Force") {
		t.Errorf("destroy does not remove the machine:\n%s", remove)
	}

	// State and notes in one call, so they cannot disagree about a machine that
	// changed between two.
	get := psGetVM("rd-dev")
	if !strings.Contains(get, "$_.State.ToString(); $_.Notes") {
		t.Errorf("state and notes are not read together:\n%s", get)
	}
	// By exact name: `Get-VM -Name` fails on a missing machine and reads the
	// name as a wildcard pattern.
	if strings.Contains(get, "-Name") || !strings.Contains(get, "$_.Name -eq 'rd-dev'") {
		t.Errorf("the machine is not looked up by exact name:\n%s", get)
	}
	if !strings.Contains(psAddress("rd-dev"), "Get-VMNetworkAdapter -VMName 'rd-dev'") {
		t.Errorf("the address is not read from the machine's adapter: %s", psAddress("rd-dev"))
	}
	if psStart("rd-dev") != "Start-VM -Name 'rd-dev'" {
		t.Errorf("start = %s", psStart("rd-dev"))
	}

	// Nothing is silenced. psScript makes every error fail the script, and an
	// error silenced by -ErrorAction still made powershell.exe exit 1, with
	// nothing printed to say why.
	for _, script := range []string{get, psAddress("rd-dev"), psStart("rd-dev"), create, remove, psAddKVP("rd-dev"), psRemoveKVP("rd-dev")} {
		if strings.Contains(script, "ErrorAction") {
			t.Errorf("an error is silenced:\n%s", script)
		}
	}

	// The key fingerprint is recorded only after the machine exists, so one
	// that failed halfway is never marked as built with a key it never got.
	notes := psSetNotes("rd-dev", hyperVNotes{Generation: "g", Key: "k"})
	if !strings.Contains(notes, "Set-VM -Name 'rd-dev' -Notes") || !strings.Contains(notes, `"key":"k"`) {
		t.Errorf("the notes command does not record what the machine was built with: %s", notes)
	}
}

// The wrapper every script runs in. powershell.exe -Command exits with whether
// its LAST statement succeeded: a missing machine asked for with -ErrorAction
// SilentlyContinue exited 1 with nothing printed, and a cmdlet failing midway
// did not stop the next one (2026-10-05).
func TestPSScript(t *testing.T) {
	got := psScript("Get-VM")
	if !strings.HasPrefix(got, "$ErrorActionPreference = 'Stop'; try { ") {
		t.Errorf("errors do not stop the script:\n%s", got)
	}
	// A script that reaches its end succeeds whatever $? says, and one that
	// does not fails, saying why.
	for _, want := range []string{"Get-VM; exit 0 }", "catch { [Console]::Error.WriteLine($_); exit 1 }"} {
		if !strings.Contains(got, want) {
			t.Errorf("psScript is missing %q:\n%s", want, got)
		}
	}
}

// Hyper-V refuses a host KVP value of 1024 characters or more, so the Ignition
// document goes over in pieces Flatcar concatenates back.
func TestIgnitionKVP(t *testing.T) {
	for _, tc := range []struct {
		size  int
		items int
	}{
		{0, 0},
		{1, 1},
		{ignitionKVPChunk, 1},
		{ignitionKVPChunk + 1, 2},
		// The last length Hyper-V accepted and the first it refused: both are
		// split, because the chunk keeps a margin below either.
		{1023, 2},
		{1024, 2},
		{2 * ignitionKVPChunk, 2},
		{2*ignitionKVPChunk + 1, 3},
	} {
		config := strings.Repeat("x", tc.size)
		items := ignitionKVP(config)
		if len(items) != tc.items {
			t.Errorf("%d bytes: %d items, want %d", tc.size, len(items), tc.items)
		}
		var joined strings.Builder
		for i, item := range items {
			if want := fmt.Sprintf("ignition.config.%d", i); item.Name != want {
				t.Errorf("%d bytes: item %d is named %q, want %q", tc.size, i, item.Name, want)
			}
			if item.Data == "" || len(item.Data) >= 1024 {
				t.Errorf("%d bytes: item %d holds %d bytes", tc.size, i, len(item.Data))
			}
			joined.WriteString(item.Data)
		}
		if joined.String() != config {
			t.Errorf("%d bytes: the items do not concatenate back to the document", tc.size)
		}
	}

	// A character is never cut in two: Hyper-V stores each value as UTF-16,
	// where half a character cannot be represented.
	config := strings.Repeat("x", ignitionKVPChunk-1) + "\u00e9" + strings.Repeat("y", 10)
	items := ignitionKVP(config)
	if len(items) != 2 || items[0].Data+items[1].Data != config {
		t.Fatalf("split into %d items", len(items))
	}
	for i, item := range items {
		if !utf8.ValidString(item.Data) {
			t.Errorf("item %d cuts a character: %q", i, item.Data)
		}
	}
}

// What psAddKVP reads on stdin has to carry the document back exactly, newlines
// and all: the unit and the host key inside it both have them.
func TestKVPInputCarriesTheDocumentIntact(t *testing.T) {
	// As long as a real ed25519 key in OpenSSH's format, about 400 bytes.
	spec := Spec{Name: "dev", Account: "dev", Port: 2222, Image: "example.com/ws:1",
		HostKey: "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("b3Blbn\t\n", 50) + "-----END OPENSSH PRIVATE KEY-----\n"}
	config, err := ignition(spec, "ssh-ed25519 AAAA dev@host")
	if err != nil {
		t.Fatal(err)
	}
	if len(config) <= ignitionKVPChunk {
		t.Fatalf("the document is %d bytes, so this test does not split it", len(config))
	}

	input, err := kvpInput(ignitionKVP(config))
	if err != nil {
		t.Fatal(err)
	}
	var items []struct{ Name, Data string }
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatalf("the input is not a JSON array of items: %v", err)
	}
	var joined strings.Builder
	for i, item := range items {
		if item.Name != fmt.Sprintf("ignition.config.%d", i) {
			t.Errorf("item %d is named %q", i, item.Name)
		}
		joined.WriteString(item.Data)
	}
	if joined.String() != config {
		t.Error("the items do not carry the document back")
	}
}

func TestPSKVP(t *testing.T) {
	add := psAddKVP("rd-dev")
	remove := psRemoveKVP("rd-dev")
	for _, script := range []string{add, remove} {
		// The embedded script, run as a script block against the machine psVM
		// finds, so psScript's error handling covers it.
		if !strings.HasPrefix(script, "& {\n") || !strings.Contains(script, "\n} -VM ("+psVM("rd-dev")+") ") {
			t.Errorf("the KVP script is not invoked as a block against the machine:\n%s", script)
		}
		for _, want := range []string{
			"param(",
			"Msvm_VirtualSystemManagementService",
			"Msvm_KvpExchangeDataItem",
			// Source 0 is the host's pool, the one Ignition reads in the guest.
			"$item.Source = 0",
			// The call can finish as a job, and only the job says whether it
			// worked.
			"$code -eq 4096",
			"$job.ErrorCode",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("the KVP script is missing %q", want)
			}
		}
	}

	if !strings.HasSuffix(add, ") -Add") {
		t.Errorf("add does not ask for -Add:\n%s", add)
	}
	for _, want := range []string{
		"AddKvpItems",
		// From stdin, never the command line: the data holds the host key.
		"[Console]::OpenStandardInput()",
		"ConvertFrom-Json",
	} {
		if !strings.Contains(add, want) {
			t.Errorf("add is missing %q", want)
		}
	}

	// Remove names the items by the prefix ignitionKVP gives them, and no data
	// reaches this command line either.
	if !strings.HasSuffix(remove, ") -Remove 'ignition.config.'") {
		t.Errorf("remove does not name the Ignition items:\n%s", remove)
	}
	if !strings.Contains(remove, "RemoveKvpItems") || !strings.Contains(remove, "HostExchangeItems") {
		t.Error("remove does not remove the items it lists")
	}

	// A refusal names the method, the item and the code, never the data.
	if !strings.Contains(kvpScript, `throw "Hyper-V refused $Method on the KVP item $Name with code $code"`) {
		t.Error("the refusal is not the one that leaves the data out")
	}
}

// A VM somebody removed by hand must not stop `rm`: stopping and removing it are
// skipped, and the disk, which is what costs the space, is still deleted.
func TestPSRemoveVMWithTheVMGone(t *testing.T) {
	remove := psRemoveVM("rd-dev", `C:\m`)

	if !strings.HasPrefix(remove, "$vm = "+psVM("rd-dev")+"; if ($vm) { Stop-VM -VM $vm -TurnOff -Force; Remove-VM -VM $vm -Force }; ") {
		t.Errorf("stopping and removing the VM is not conditional on finding it:\n%s", remove)
	}
	if !strings.HasSuffix(remove, `}; if (Test-Path -LiteralPath 'C:\m') { Remove-Item -LiteralPath 'C:\m' -Recurse -Force }`) {
		t.Errorf("the disk is not deleted whether or not the VM was there:\n%s", remove)
	}
}
