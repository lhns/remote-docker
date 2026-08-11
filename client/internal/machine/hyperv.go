package machine

// The Hyper-V backend's decisions, separated from running anything.
//
// The same split as the WSL backend and for a stronger reason: nobody working
// on this project has WSL, and NOBODY ANYWHERE can run this one in CI. GitHub's
// runners do not offer Hyper-V, so `docs/testing-machines.md` is its whole
// verification and every line that can be a pure function of a string is one.
//
// What runs here is Flatcar Container Linux with the workspace image as a
// privileged container, which is the compose deployment unchanged (ADR 0026).
// Flatcar is chosen for the property the whole design rests on: it has no
// package manager, /usr is immutable, and its entire configuration is one
// declarative Ignition file applied at first boot. There is no `apt install`
// to be halfway through when the power goes.
// (Checked 2026-08-11: `curl -sI https://stable.release.flatcar-linux.net/\
// amd64-usr/current/flatcar_production_hyperv_image.vhd.bz2` and
// https://www.flatcar.org/docs/latest/installing/vms/hyper-v/. Fedora CoreOS is
// the equivalent alternative if that stops being true.)

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// hyperVSwitch is the network the machine is attached to.
//
// The Default Switch, which Hyper-V creates and maintains itself: it is NAT
// with DHCP, the host can reach machines on it, and it needs no administrator
// decision about the user's network. A machine gets a new address from it on
// every boot, which is why an address is asked for rather than stored -- the
// same answer the WSL backend arrived at by measurement.
const hyperVSwitch = "Default Switch"

// hyperVNotes is what a machine records about itself, in the one place Hyper-V
// offers for it.
//
// The VM's Notes field, because there is nowhere else. A Linux guest cannot be
// read from the host the way `wsl -d x -- cat` reads a distribution: Hyper-V's
// PowerShell Direct is Windows-guest only, so a file inside this machine is
// unreachable until the machine is up and answering, which is exactly when the
// question is already moot.
type hyperVNotes struct {
	Generation string `json:"generation"`

	// Key is the fingerprint of the public key baked in at creation. See
	// hyperVEnrolment.
	Key string `json:"key,omitempty"`
}

// encodeNotes and decodeNotes carry hyperVNotes through the Notes field.
//
// JSON on one line, because Notes is free text a person may also have typed in,
// and something obviously machine-written is kinder than key=value that reads
// like prose. A Notes field that cannot be parsed is treated as a machine with
// no generation, which Plan reads as a match -- deliberately, since recreating
// somebody's machine over an unreadable string would take their containers with
// it.
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
// A Hyper-V machine takes its key at creation and cannot be given another one
// later. There is no way in: the guest is Linux, so PowerShell Direct does not
// apply, and the only door is the SSH the key is for. This is a real asymmetry
// with the WSL backend, where enrolling is a file write, and it is reported
// rather than papered over -- silently accepting a key that will not work is
// the failure that succeeds.
//
// It is deliberately NOT part of the generation. A changed key would then mean
// a rebuild happening on its own, and a rebuild discards every image in the
// machine (ADR 0026).
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
// several others. Anything that is not plainly Running is reported Stopped,
// because the only question asked is whether to start it, and Start on a saved
// or paused machine resumes it correctly.
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

// parseVMAddress picks the address to reach a machine at.
//
// Get-VMNetworkAdapter reports every address the guest told Hyper-V about,
// through the integration services: IPv4, IPv6, and often a link-local pair
// that is useless from here. The first IPv4 that is not link-local is the
// answer; link-local (169.254/16) means DHCP has not finished, which is a
// machine that is up but not ready rather than one to connect to.
func parseVMAddress(raw string) string {
	return firstIPv4(strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}))
}

// ignition is the machine's entire configuration, applied at first boot.
//
// One declarative document and no second step: this is what makes a Hyper-V
// machine defined by its Spec rather than by whatever happened to it since. It
// starts the workspace image as a privileged container with host networking,
// which is the compose deployment (ADR 0026) with the compose file spelled
// differently.
//
// Host networking rather than a published port because the agent also binds a
// port per enrolled account for its reverse tunnels (the uid->port formula),
// and publishing a range that the formula decides would put the formula in two
// places -- the mistake ADR 0011 exists to record.
func ignition(spec Spec, publicKey string) (string, error) {
	unit := hyperVUnit(spec)

	doc := map[string]any{
		"ignition": map[string]any{"version": "3.4.0"},
		"storage": map[string]any{
			"files": []any{
				// The account's key, written where the agent's watcher looks.
				// This is the whole of enrolment on this backend, and the
				// reason it happens here is that afterwards there is no way in
				// (see hyperVEnrolment).
				map[string]any{
					"path":      "/etc/workspace/authorized_keys.d/" + spec.Account + ".pub",
					"mode":      0o600,
					"overwrite": true,
					"contents": map[string]any{
						"source": "data:," + urlEncode(strings.TrimSpace(publicKey)+"\n"),
					},
				},
			},
		},
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

// hyperVUnit is the systemd unit that runs the workspace.
//
// Restart=always and no `--rm`: the container holds the machine's docker state,
// and a restart that discarded it would lose every image the user built. This
// is the same argument that keeps `--rm` off a per-account daemon.
func hyperVUnit(spec Spec) string {
	image := spec.Image
	if image == "" {
		// Only when a Spec reached here without one, which createMachine does
		// not allow. The unversioned tag is the honest fallback: nothing here
		// knows which client asked.
		image = DefaultImageRepo + ":latest"
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
// Written out rather than url.QueryEscape, which encodes a space as `+` -- and
// a `+` in an SSH key is a different key. Ignition reads these as data URLs,
// not as query strings.
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

// The PowerShell each operation runs.
//
// Built as strings here so the commands themselves are testable on a machine
// with no Hyper-V, which is every machine this is developed on. Each takes the
// VM name already prefixed: a bare name reaching Get-VM is a machine somebody
// else made.

// psGetVM asks for a machine's state and its notes in one call.
//
// One call rather than two, so the state and the generation cannot disagree
// about a machine that changed between them.
func psGetVM(vm string) string {
	return fmt.Sprintf(
		"$vm = Get-VM -Name %s -ErrorAction SilentlyContinue; "+
			"if ($vm) { $vm.State.ToString(); $vm.Notes }", psQuote(vm))
}

// psAddress asks the guest, through the integration services, what address it
// was given.
func psAddress(vm string) string {
	return fmt.Sprintf(
		"(Get-VMNetworkAdapter -VMName %s -ErrorAction SilentlyContinue).IPAddresses -join ','", psQuote(vm))
}

// psNewVM creates the machine and attaches its disk.
//
// Generation 2 is the UEFI one, which is what Flatcar's Hyper-V image expects.
// Secure boot is off because that image is not signed by a certificate Hyper-V
// ships, and a machine that will not boot is a poorer answer than one that
// boots unsigned code the user chose to download.
func psNewVM(vm, vhd, dir string, spec Spec) string {
	cmd := []string{
		fmt.Sprintf("New-VM -Name %s -Generation 2 -VHDPath %s -Path %s -SwitchName %s",
			psQuote(vm), psQuote(vhd), psQuote(dir), psQuote(hyperVSwitch)),
		fmt.Sprintf("Set-VMFirmware -VMName %s -EnableSecureBoot Off", psQuote(vm)),
		fmt.Sprintf("Set-VM -Name %s -Notes %s -AutomaticStartAction Nothing -CheckpointType Disabled",
			psQuote(vm), psQuote(encodeNotes(hyperVNotes{Generation: spec.Generation()}))),
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

// psSetNotes records what a machine was built from and with.
func psSetNotes(vm string, notes hyperVNotes) string {
	return fmt.Sprintf("Set-VM -Name %s -Notes %s", psQuote(vm), psQuote(encodeNotes(notes)))
}

// psRemoveVM destroys a machine and the disk under it.
//
// The disk is deleted explicitly: Remove-VM leaves it, which would silently
// keep gigabytes per machine somebody thought they had removed. Stop first with
// -TurnOff, because a machine being destroyed has nothing to flush and a clean
// shutdown can wait indefinitely on a guest that is not listening.
func psRemoveVM(vm, dir string) string {
	return fmt.Sprintf(
		"Stop-VM -Name %s -TurnOff -Force -ErrorAction SilentlyContinue; "+
			"Remove-VM -Name %s -Force; "+
			"Remove-Item -LiteralPath %s -Recurse -Force -ErrorAction SilentlyContinue",
		psQuote(vm), psQuote(vm), psQuote(dir))
}

// psQuote wraps a string as a PowerShell single-quoted literal.
//
// Doubling is how a single quote is escaped there, and it is the only escape
// inside such a literal -- which is the point of using one: nothing in it is
// expanded, so a `$` in a machine's notes stays a `$`.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
