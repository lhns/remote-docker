package machine

// The WSL backend's decisions, separated from running anything.
//
// Everything here compiles and is tested on every platform; wsl_windows.go is
// the thin part that actually executes wsl.exe. That split is not tidiness:
// nobody working on this project has WSL, so the more of it that is a pure
// function of a string, the more of it has been run before it ships.
//
// The two things that are genuinely easy to get wrong are both here. wsl.exe
// writes UTF-16, so its output is not the ASCII it looks like in a terminal.
// And `wsl -l -v` marks the default distribution with an asterisk in the first
// column, so the name of a default distribution is not the first field.

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf16"
)

// WSLName is the distribution name for a machine.
//
// Prefixed, because a WSL distribution list is the user's own namespace: it
// holds their Ubuntu, their Docker Desktop distributions, whatever else. Taking
// a bare name there is the same mistake as taking a bare unix account name on
// a VM (ADR 0025).
func WSLName(machine string) string { return "rd-" + machine }

// generationFile is where a machine's generation is kept, inside the
// distribution itself.
//
// Inside rather than beside: a distribution exported, moved or re-imported by
// hand carries it, and a distribution somebody deleted takes it with it. The
// alternative -- a file next to the config -- can disagree with reality, and a
// generation that lies is worse than one that is missing, because Plan trusts
// a mismatch enough to recreate.
const generationFile = "/etc/remote-docker-generation"

// agentLog is where the agent's output goes inside the machine.
//
// WSL's boot command has no console and nothing collects its output, so without
// this an agent that refuses to start is a machine that is simply unreachable,
// with the reason written to a closed file descriptor. It is a shell
// redirection because the boot command is run by a shell.
const agentLog = "/var/log/remote-dockerd.log"

// decodeWSLOutput turns wsl.exe's output into a string.
//
// wsl.exe writes UTF-16LE with a BOM. Read as bytes and printed, it looks like
// text with NULs between every character, and every naive `strings.Contains`
// against it fails for a reason nothing on screen explains. Older builds write
// plain ASCII, so both have to work.
func decodeWSLOutput(raw []byte) string {
	if len(raw) >= 2 && raw[0] == 0xff && raw[1] == 0xfe {
		raw = raw[2:]
	} else if !bytes.Contains(raw, []byte{0x00}) {
		// No BOM and no NULs: this build speaks ASCII.
		return string(raw)
	}

	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// WSLDistribution is one row of `wsl -l -v`.
type WSLDistribution struct {
	Name    string
	State   string
	Version int
}

// parseWSLList reads `wsl --list --verbose` output.
//
// The asterisk marking the default distribution is a column, not part of the
// name, and a machine that happens to be the user's default would otherwise be
// called "*rd-dev" and never found.
func parseWSLList(raw []byte) []WSLDistribution {
	var out []WSLDistribution

	for line := range strings.SplitSeq(decodeWSLOutput(raw), "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
		if line == "" {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "*"))

		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// The header, in whatever language this Windows speaks. Recognised by
		// its shape rather than its words: the last column of a real row is a
		// number and the header's never is.
		version := 0
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &version); err != nil {
			continue
		}
		out = append(out, WSLDistribution{
			Name:    fields[0],
			State:   fields[1],
			Version: version,
		})
	}
	return out
}

// observeWSL turns a distribution list into what Plan needs.
//
// A version 1 distribution is reported as absent, deliberately. It cannot run
// the agent -- there is no real kernel under it, so there is no dockerd and no
// NFS mount -- and calling it "stopped" would send the caller into `start`,
// which would fail with something obscure. Absent means `create`, and create
// says what is wrong.
func observeWSL(distros []WSLDistribution, name, generation string) Observed {
	for _, d := range distros {
		if !strings.EqualFold(d.Name, name) {
			continue
		}
		if d.Version < 2 {
			return Observed{State: Absent}
		}
		state := Stopped
		if strings.EqualFold(d.State, "Running") {
			state = Running
		}
		return Observed{State: state, Generation: generation}
	}
	return Observed{State: Absent}
}

// wslConf is the distribution's own configuration.
//
// `[boot] command` is what starts the agent, and it is why nothing here
// supervises anything: WSL runs it every time the distribution starts, so a
// machine that was terminated, rebooted or shut down comes back with the agent
// running and no help from this program. A supervisor on the Windows side
// would be a second thing to keep alive, and the first thing to be missing
// after a reboot.
//
// systemd is off: the agent is the only thing that has to run, it does its own
// dockerd supervision (ADR 0010), and systemd inside WSL is one more moving
// part between a user and a working daemon.
func wslConf(spec Spec) string {
	env := []string{
		// The image's own environment, restored by hand.
		//
		// `docker export` writes a FILESYSTEM. The image config -- ENV, PATH,
		// the entrypoint -- lives beside the layers and is not in the tarball,
		// so a machine imported from one starts with WSL's environment and none
		// of the image's. It fails a long way from here: the agent starts,
		// cannot find `dockerd-entrypoint.sh` on a PATH without /usr/local/bin,
		// restarts it every two seconds forever, and blocks its own listener for
		// ninety seconds waiting for a socket that will never appear. What
		// Windows sees is a refused connection.
		//
		// PATH and DOCKER_TLS_CERTDIR are the two that have no default in the
		// agent's own code. DOCKER_TLS_CERTDIR is EMPTY on purpose: it is how
		// image/Dockerfile turns dind's TLS off, and unset is not the same
		// answer as empty -- dind generates certificates and listens on 2376
		// instead.
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_TLS_CERTDIR=",

		"WORKSPACE_STATE_DIR=/etc/workspace",
		"WORKSPACE_KEYS_DIR=/etc/workspace/authorized_keys.d",
		"WORKSPACE_HOSTKEY_DIR=/etc/workspace/host_keys",
		"WORKSPACE_ENABLE_DIND=true",

		// One daemon, not one per account. A machine on somebody's own computer
		// has exactly one account -- them -- so a daemon each separates nobody
		// from anybody, and ADR 0012's argument for the shared daemon applies
		// in full: it would cost a nested dind container, a second graph store
		// and a duplicated layer cache to give the user privacy from
		// themselves. It also stops the workspace's own daemon standing between
		// a fresh machine and its first connection: with a daemon per account
		// the agent adopts running daemons before it serves, and adoption asks
		// dockerd.
		"WORKSPACE_PER_USER_DIND=false",
	}
	// Bound to every interface INSIDE the machine, not to its loopback.
	//
	// WSL2's default networking is NAT, and Windows reaches the machine either
	// through its localhost relay or at the machine's own address. A service on
	// the machine's loopback can only ever be reached by the first of those, so
	// binding wider costs nothing and keeps the second possible.
	//
	// Not an exposure: that interface is a host-only virtual network, and the
	// agent authenticates every connection by key wherever it came from.
	return "[boot]\nsystemd=false\ncommand=/usr/bin/env " +
		strings.Join(env, " ") +
		fmt.Sprintf(" /usr/local/bin/remote-dockerd serve --addr :%d >>%s 2>&1\n", spec.Port, agentLog)
}

// wslAddressArgs asks a distribution for its own address.
//
// eth0 by name: WSL2 gives a distribution one NAT interface, and reading the
// route table instead would answer with the gateway, which is the Windows side.
func wslAddressArgs(name string) []string {
	return wslRunArgs(name, "ip", "-4", "-o", "addr", "show", "eth0")
}

// parseWSLAddress reads an address out of `ip -4 -o addr show eth0`.
//
// Written here rather than as a shell pipeline in the command: `hostname -I`
// does not exist in busybox, and an awk field passed through two layers of
// quoting is a thing that fails silently and returns the whole line.
func parseWSLAddress(raw []byte) string {
	fields := strings.Fields(decodeWSLOutput(raw))
	for i, f := range fields {
		if f != "inet" || i+1 >= len(fields) {
			continue
		}
		// inet 172.24.110.158/20 -- the mask is the machine's, not ours.
		return strings.TrimSpace(strings.SplitN(fields[i+1], "/", 2)[0])
	}
	return ""
}

// wslReadGenerationArgs reads a machine's generation from inside it.
func wslReadGenerationArgs(name string) []string {
	return wslRunArgs(name, "cat", generationFile)
}

// wslWriteArgs writes a file inside a distribution.
//
// printf rather than a heredoc or tee: one process, no shell features beyond
// quoting, and nothing that behaves differently between the shells a rootfs
// might ship.
func wslWriteArgs(name, path, content string) []string {
	return wslRunArgs(name, "sh", "-c", "printf '%s' "+shellQuote(content)+" > "+path)
}

// shellQuote wraps a string for `sh -c`.
//
// Single quotes, with the only escape sh understands for them: end the quote,
// an escaped quote, start again. Nothing passed here is hostile -- it is our
// own config and a hex digest -- so this exists so that a newline in the
// content does not end the command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wslImportArgs is the command that creates a distribution from a rootfs.
func wslImportArgs(name, dir, rootfs string, version int) []string {
	return []string{"--import", name, dir, rootfs, "--version", fmt.Sprint(version)}
}

// wslRunArgs runs a command inside a distribution as root.
//
// --user root and --cd / are both deliberate. WSL runs as the default user of
// the distribution, which for an imported rootfs is root but need not stay so,
// and it starts in the Windows working directory translated into /mnt, which
// is a path that may not exist and is never what the agent wants.
func wslRunArgs(name string, command ...string) []string {
	return append([]string{"-d", name, "--user", "root", "--cd", "/", "--"}, command...)
}
