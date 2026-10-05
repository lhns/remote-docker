package machine

// The Hyper-V backend's decisions, separated from running anything.
//
// The same split as the WSL backend and for a stronger reason: GitHub's runners
// do not offer Hyper-V, so `docs/testing-machines.md` is its whole verification
// and every line that can be a pure function of a string is one. It has been
// run by hand once, on 2026-10-05 (ADR 0026).
//
// What runs here is Flatcar Container Linux with the workspace image as a
// privileged container, which is the compose deployment unchanged (ADR 0026).
// Flatcar for the property the design rests on: no package manager, an
// immutable /usr, and one Ignition document applied at first boot.
// (Checked 2026-10-05: `curl -sI https://stable.release.flatcar-linux.net/\
// amd64-usr/current/flatcar_production_hyperv_vhdx_image.vhdx.zip` and
// https://www.flatcar.org/docs/latest/deploy/virt-options/hyper-v/. Fedora
// CoreOS is the equivalent alternative if that stops being true.)

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// hyperVSwitch is the network the machine is attached to: the Default Switch,
// which Hyper-V creates and maintains itself, so it needs no administrator
// decision about the user's network. It is NAT with DHCP, and a machine gets a
// new address on every boot, which is why Backend.Address is asked every time.
const hyperVSwitch = "Default Switch"

// hyperVBuilding is the generation a machine carries between New-VM and
// psSetNotes: one no Spec produces, so Plan reads a create that died half way
// as Recreate. The Spec's own generation would read as finished, and none at
// all as a match (Observed.Generation), both a machine nothing can log into.
const hyperVBuilding = "building"

// hyperVNotes is what a machine records about itself, in the one place Hyper-V
// offers for it.
//
// The VM's Notes field, because there is nowhere else. Hyper-V's PowerShell
// Direct is Windows-guest only, so a file inside this Linux guest cannot be
// read the way `wsl -d x -- cat` reads a distribution: it is unreachable until
// the machine is up and answering, which is when the question is already moot.
type hyperVNotes struct {
	Generation string `json:"generation"`

	// Key is the fingerprint of the public key baked in at creation. See
	// hyperVEnrolment.
	Key string `json:"key,omitempty"`
}

// encodeNotes and decodeNotes carry hyperVNotes through the Notes field.
//
// JSON on one line, because Notes is free text a person may also have typed in
// and something obviously machine-written is kinder than key=value that reads
// like prose. Notes that cannot be parsed become a machine with no generation,
// which Plan reads as a match. See Observed.Generation.
func encodeNotes(n hyperVNotes) string {
	raw, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeNotes(raw string) hyperVNotes {
	var n hyperVNotes
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &n); err != nil {
		return hyperVNotes{}
	}
	return n
}

// keyFingerprint identifies a public key without storing it.
//
// The stored answer sits in a VM's Notes, which is not a secret and not a
// place to put one. A fingerprint answers the only question asked of it --
// whether this is the same key the machine was built with -- and answers
// nothing else.
func keyFingerprint(publicKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(publicKey)))
	return hex.EncodeToString(sum[:])[:16]
}

// hyperVEnrolment says whether a key already reaches this machine.
//
// A Hyper-V machine takes its key at creation and cannot be given another one:
// PowerShell Direct is Windows-guest only, so the only door is the SSH the key
// is for. That is a real asymmetry with the WSL backend, where enrolling is a
// file write, and it is reported rather than papered over. Silently accepting a
// key that cannot work is the failure that succeeds.
func hyperVEnrolment(stored hyperVNotes, publicKey string) error {
	want := keyFingerprint(publicKey)
	if stored.Key == "" || stored.Key == want {
		// Empty means a machine from before this was recorded. Assumed to
		// match, because refusing every older machine over a fingerprint
		// nobody wrote down is worse than the connection error a wrong key
		// gives, which says what it is.
		return nil
	}
	return fmt.Errorf("this machine was built with a different key\n" +
		"  fix: `remote machine rebuild` to build it with this one, which discards its images and containers")
}

// parseVMState reads the State column of `Get-VM`.
//
// Hyper-V has more states than this design has: Saved, Paused, Starting and
// several others. Anything not plainly Running is reported Stopped, because the
// only question asked is whether to start it and Start-VM resumes a saved or
// paused machine correctly.
func parseVMState(raw string) State {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return Absent
	case "running":
		return Running
	default:
		return Stopped
	}
}

// observeVM turns what look read into what Plan is asked about. A PowerShell
// failure is returned, not folded into Absent, which would send `create` at a
// running machine; absence itself is psGetVM printing nothing.
func observeVM(state State, notes hyperVNotes, err error) (Observed, error) {
	if err != nil {
		return Observed{}, err
	}
	if state == Absent {
		return Observed{State: Absent}, nil
	}
	return Observed{State: state, Generation: notes.Generation}, nil
}

// parseVMAddress picks the address to reach a machine at.
//
// Get-VMNetworkAdapter reports every address the guest told Hyper-V about
// through the integration services: IPv4, IPv6, and often a link-local pair.
// firstIPv4 picks, and says why link-local is skipped.
func parseVMAddress(raw string) string {
	return firstIPv4(strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}))
}

// ignition is the machine's entire configuration, applied at first boot and
// delivered over KVP (ignitionKVP).
//
// One declarative document and no second step, which is what makes a Hyper-V
// machine defined by its Spec rather than by whatever happened to it since.
//
// Host networking rather than a published port: the agent also binds a port per
// enrolled account for its reverse tunnels, from the uid->port formula, and
// publishing a range that formula decides would put the formula in two places
// (ADR 0021).
func ignition(spec Spec, publicKey string) (string, error) {
	unit := hyperVUnit(spec)

	files := []any{
		// The account's key, written where the agent's watcher looks. This is
		// the whole of enrolment on this backend, and the reason it happens
		// here is that afterwards there is no way in (see hyperVEnrolment).
		ignitionFile("/etc/workspace/authorized_keys.d/"+spec.Account+".pub", strings.TrimSpace(publicKey)+"\n"),
	}
	// hyperVUnit mounts /etc/workspace into the workspace container.
	if spec.HostKey != "" {
		files = append(files, ignitionFile(hostKeyFile, spec.HostKey))
	}

	doc := map[string]any{
		"ignition": map[string]any{"version": "3.4.0"},
		"storage":  map[string]any{"files": files},
		"systemd": map[string]any{
			"units": []any{
				map[string]any{
					"name":     "remote-dockerd.service",
					"enabled":  true,
					"contents": unit,
				},
			},
		},
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("building the machine's configuration: %w", err)
	}
	return string(raw), nil
}

// ignitionFile is one file Ignition writes, readable by root alone.
func ignitionFile(path, content string) map[string]any {
	return map[string]any{
		"path":      path,
		"mode":      0o600,
		"overwrite": true,
		"contents":  map[string]any{"source": "data:," + urlEncode(content)},
	}
}

// hyperVUnit is the systemd unit that runs the workspace.
//
// Restart=always and no `--rm`: the container holds the machine's docker state,
// and a restart that discarded it would lose every image the user built. This
// is the same argument that keeps `--rm` off a per-account daemon.
func hyperVUnit(spec Spec) string {
	image := spec.Image
	if image == "" {
		// Only when a Spec reached here without one, which
		// client/cmd/remote-docker's createMachine does not allow. The
		// unversioned tag is the honest fallback: nothing here knows which
		// client asked.
		image = defaultImageRepo + ":latest"
	}

	return strings.Join([]string{
		"[Unit]",
		"Description=remote-docker workspace",
		"After=docker.service",
		"Requires=docker.service",
		"",
		"[Service]",
		"Restart=always",
		"RestartSec=5",
		"ExecStartPre=-/usr/bin/docker rm -f remote-dockerd",
		"ExecStart=/usr/bin/docker run --name remote-dockerd --privileged --network host " +
			"-v /etc/workspace:/etc/workspace -v rd-lib:/var/lib/docker " +
			"-e WORKSPACE_PER_USER_DIND=false " +
			fmt.Sprintf("%s serve --addr :%d", image, spec.Port),
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

// urlEncode percent-encodes for a data: URL.
//
// Written out rather than url.QueryEscape, which encodes a space as `+`, and a
// `+` in an SSH key is a different key. Ignition reads these as data URLs, not
// as query strings.
func urlEncode(s string) string {
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"

	var b strings.Builder
	for _, c := range []byte(s) {
		if strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// ignitionKVPChunk is the longest value one KVP item carries to the guest.
//
// Hyper-V refuses a host-to-guest value of 1024 characters or more (the job
// ends with ErrorCode 32773; 1023 was accepted), measured 2026-10-05 on vmms
// 10.0.26100.8875. 1000 leaves a margin, and is what the first machine that
// booted with its configuration applied was built with. Counted in bytes, which
// are never fewer than the UTF-16 characters Hyper-V counts.
const ignitionKVPChunk = 1000

// kvpItem is one key-value pair handed to the guest through Hyper-V's KVP
// exchange.
type kvpItem struct{ Name, Data string }

// ignitionKVP splits the Ignition document into the KVP items Flatcar reads it
// back from: `ignition.config.0`, `ignition.config.1` and so on, which its
// Hyper-V provider concatenates in order. This is how Flatcar's Hyper-V image
// takes a configuration (`kvpctl add-ign` in containers/libhvee does the same),
// and the only way: a file beside the disk is never read (ADR 0026).
//
// Cut at rune boundaries, so no item holds half a character it cannot encode.
func ignitionKVP(config string) []kvpItem {
	var items []kvpItem
	for len(config) > 0 {
		n := min(ignitionKVPChunk, len(config))
		for n < len(config) && !utf8.RuneStart(config[n]) {
			n--
		}
		items = append(items, kvpItem{Name: fmt.Sprintf("ignition.config.%d", len(items)), Data: config[:n]})
		config = config[n:]
	}
	return items
}

// kvpInput is what psAddKVP reads on its stdin: one item per line, the name and
// the data separated by a tab.
//
// Stdin rather than the command line, because the data holds the machine's
// private host key and a command line is readable by every process on this
// computer. Neither separator can occur in the data: the Ignition document is
// JSON from json.Marshal, which escapes both inside strings and emits neither
// between tokens.
func kvpInput(items []kvpItem) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(item.Name + "\t" + item.Data + "\n")
	}
	return b.String()
}

// The PowerShell each operation runs. Each takes the VM name ALREADY PREFIXED:
// a bare name reaching Get-VM is a machine somebody else made.

// psScript is how every one of them is run: any error stops the script and
// fails it, and a script that reaches its end succeeds.
//
// powershell.exe -Command exits with whether its LAST statement succeeded,
// which is not whether the script did. A cmdlet told -ErrorAction
// SilentlyContinue still leaves $? false, so asking for a machine that was not
// there exited 1 with nothing printed, and `machine create` failed every time
// with "cannot tell what is there: exit status 1:" (2026-10-05). The other
// direction is as wrong: a cmdlet failing mid-script did not stop the next one.
func psScript(body string) string {
	return "$ErrorActionPreference = 'Stop'; try { " + body +
		"; exit 0 } catch { [Console]::Error.WriteLine($_); exit 1 }"
}

// psVM finds a machine by exact name, and finds nothing when it is not there.
//
// Filtered from the whole list rather than asked for with `Get-VM -Name`,
// which fails on a missing machine and reads the name as a wildcard pattern.
// Absence is then an empty answer, and every error is a real one, such as not
// being allowed to ask.
func psVM(vm string) string {
	return fmt.Sprintf("Get-VM | Where-Object { $_.Name -eq %s } | Select-Object -First 1", psQuote(vm))
}

// psGetVM asks for a machine's state and its notes in one call, and prints
// nothing for a machine that is not there.
//
// One call rather than two, so the state and the generation cannot disagree
// about a machine that changed between them.
func psGetVM(vm string) string {
	return psVM(vm) + " | ForEach-Object { $_.State.ToString(); $_.Notes }"
}

// psStart starts a machine. Start-VM on one already running only warns, which
// is what lets Locate start rather than inspect first. On a missing one it
// fails, and that is not silenced: Locate would otherwise wait out its whole
// budget for an address from a machine somebody removed by hand.
func psStart(vm string) string {
	return fmt.Sprintf("Start-VM -Name %s", psQuote(vm))
}

// psAddress asks the guest, through the integration services, what address it
// was given. Its KVP daemon reports one about 30 seconds after Start-VM and
// nothing until then (2026-10-05), which Locate waits out.
func psAddress(vm string) string {
	return fmt.Sprintf("(Get-VMNetworkAdapter -VMName %s).IPAddresses -join ','", psQuote(vm))
}

// psNewVM creates the machine and attaches its disk.
//
// Generation 2 is the UEFI one, which is what Flatcar's Hyper-V image expects.
// Secure boot is off because that image is not signed by a certificate Hyper-V
// ships, and a machine that will not boot is a poorer answer than one that
// boots unsigned code the user chose to download.
func psNewVM(vm, vhd, dir string, spec Spec) string {
	cmd := []string{
		fmt.Sprintf("New-VM -Name %s -Generation 2 -VHDPath %s -Path %s -SwitchName %s | Out-Null",
			psQuote(vm), psQuote(vhd), psQuote(dir), psQuote(hyperVSwitch)),
		fmt.Sprintf("Set-VMFirmware -VMName %s -EnableSecureBoot Off", psQuote(vm)),
		// Marked as unfinished, not as built: psSetNotes is the single point
		// at which a machine becomes "built". See hyperVBuilding.
		fmt.Sprintf("Set-VM -Name %s -Notes %s -AutomaticStartAction Nothing -CheckpointType Disabled",
			psQuote(vm), psQuote(encodeNotes(hyperVNotes{Generation: hyperVBuilding}))),
	}
	// Zero means the platform's own default, which is a better number than one
	// invented here.
	if spec.CPUs > 0 {
		cmd = append(cmd, fmt.Sprintf("Set-VMProcessor -VMName %s -Count %d", psQuote(vm), spec.CPUs))
	}
	if spec.MemoryMB > 0 {
		cmd = append(cmd, fmt.Sprintf("Set-VMMemory -VMName %s -StartupBytes %dMB", psQuote(vm), spec.MemoryMB))
	}
	return strings.Join(cmd, "; ")
}

// psAddKVP hands the items kvpInput wrote on stdin to the machine, through the
// WMI call `kvpctl add-ign` makes. Before the machine first starts, because
// Ignition reads them once, at first boot.
//
// Hyper-V has no cmdlet for this: it is
// Msvm_VirtualSystemManagementService.AddKvpItems with a
// Msvm_KvpExchangeDataItem whose Source 0 means "from the host". The call may
// finish as a job (4096), and only the job says whether it worked. The error
// names the item and the code, never the data, which holds the host key.
func psAddKVP(vm string) string {
	const ns = `root\virtualization\v2`
	return strings.Join([]string{
		"$vm = " + psVM(vm),
		"if (-not $vm) { throw " + psQuote("no machine named "+vm) + " }",
		"$cs = Get-WmiObject -Namespace " + psQuote(ns) + " -Class Msvm_ComputerSystem -Filter \"Name='$($vm.Id)'\"",
		"$svc = Get-WmiObject -Namespace " + psQuote(ns) + " -Class Msvm_VirtualSystemManagementService",
		"$in = New-Object IO.StreamReader([Console]::OpenStandardInput(), (New-Object Text.UTF8Encoding $false))",
		"while ($null -ne ($line = $in.ReadLine())) { " +
			"$name, $data = $line -split \"`t\", 2; " +
			"$item = ([wmiclass]" + psQuote(ns+":Msvm_KvpExchangeDataItem") + ").CreateInstance(); " +
			"$item.Name = $name; $item.Data = $data; $item.Source = 0; " +
			"$r = $svc.AddKvpItems($cs, @($item.PSBase.GetText(1))); $code = $r.ReturnValue; " +
			"if ($code -eq 4096) { $job = [wmi]$r.Job; " +
			"while ($job.JobState -lt 7) { Start-Sleep -Milliseconds 200; $job = [wmi]$r.Job }; " +
			"$code = $job.ErrorCode }; " +
			"if ($code -ne 0) { throw \"Hyper-V refused the KVP item $name with code $code\" } }",
	}, "; ")
}

// psSetNotes records what a machine was built from and with.
func psSetNotes(vm string, notes hyperVNotes) string {
	return fmt.Sprintf("Set-VM -Name %s -Notes %s", psQuote(vm), psQuote(encodeNotes(notes)))
}

// psRemoveVM destroys a machine and the disk under it.
//
// The disk is deleted explicitly: Remove-VM leaves it, which would silently
// keep gigabytes per machine somebody thought they had removed, so a disk that
// cannot be deleted is an error. Stop first with -TurnOff, because a machine
// being destroyed has nothing to flush and a clean shutdown can wait
// indefinitely on a guest that is not listening; on one already off it only
// warns.
func psRemoveVM(vm, dir string) string {
	return fmt.Sprintf(
		"Stop-VM -Name %s -TurnOff -Force; "+
			"Remove-VM -Name %s -Force; "+
			"if (Test-Path -LiteralPath %s) { Remove-Item -LiteralPath %s -Recurse -Force }",
		psQuote(vm), psQuote(vm), psQuote(dir), psQuote(dir))
}

// psQuote wraps a string as a PowerShell single-quoted literal.
//
// Doubling is how a single quote is escaped there and the only escape such a
// literal has, which is the point of using one: nothing in it is expanded, so a
// `$` in a machine's notes stays a `$`.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
