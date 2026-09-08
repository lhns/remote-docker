package machine

// The WSL backend's decisions, tested on every platform. wsl_windows.go is the
// thin part that executes wsl.exe.
//
// The two things easy to get wrong are decodeWSLOutput and parseWSLList below,
// and both fail silently.

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf16"
)

// generationFile is where a machine's generation is kept, inside the
// distribution itself.
//
// Inside rather than beside: a distribution exported, moved or re-imported by
// hand carries it, and one somebody deleted takes it with it. A file next to
// the config can disagree with reality, and a generation that lies is worse
// than one that is missing, because Plan trusts a mismatch enough to recreate.
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
// wsl.exe writes UTF-16LE with a BOM. Read as bytes it looks like text with a
// NUL between every character, and every `strings.Contains` against it fails
// for a reason nothing on screen explains. Older builds write plain ASCII, so
// both have to work.
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

// wslDistribution is one row of `wsl -l -v`.
type wslDistribution struct {
	Name    string
	State   string
	Version int
}

// parseWSLList reads `wsl --list --verbose` output.
//
// The asterisk marking the default distribution is a column, not part of the
// name, and a machine that happens to be the user's default would otherwise be
// called "*rd-dev" and never found.
func parseWSLList(raw []byte) []wslDistribution {
	var out []wslDistribution

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
		out = append(out, wslDistribution{
			Name:    fields[0],
			State:   fields[1],
			Version: version,
		})
	}
	return out
}

// observeWSL turns a distribution list into what Plan needs.
//
// A version 1 distribution is reported as absent, deliberately. It has no real
// kernel under it, so it can run neither dockerd nor an NFS mount, and calling
// it "stopped" would send the caller into `start` to fail obscurely. Absent
// means `create`, which says what is wrong.
func observeWSL(distros []wslDistribution, name, generation string) Observed {
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
// `[boot] command` is what starts the agent, and it is why nothing on the
// Windows side supervises anything: WSL runs it every time the distribution
// starts, so a machine that was terminated, rebooted or shut down comes back
// with the agent running.
//
// systemd is off: the agent is the only thing that has to run and it supervises
// dockerd itself (ADR 0010).
func wslConf(spec Spec) string {
	env := []string{
		// The image's own environment, restored by hand.
		//
		// `docker export` writes a FILESYSTEM; the image config (ENV, PATH, the
		// entrypoint) lives beside the layers and is not in the tarball, so a
		// machine imported from one starts with WSL's environment and none of
		// the image's. It fails a long way from here: the agent cannot find
		// `dockerd-entrypoint.sh` on a PATH without /usr/local/bin, restarts it
		// every two seconds forever, and blocks its own listener for ninety
		// seconds waiting for a socket that never appears. Windows sees a
		// refused connection.
		//
		// PATH and DOCKER_TLS_CERTDIR are the two with no default in the agent's
		// own code. DOCKER_TLS_CERTDIR is EMPTY rather than unset, which is
		// still a different answer: the agent names `dockerd` itself, so the
		// dind block that reads this never runs, but dind's docker-entrypoint.sh
		// does, and a non-empty value there points a client with no DOCKER_HOST
		// and no socket at tcp://docker:2376.
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_TLS_CERTDIR=",

		"WORKSPACE_STATE_DIR=/etc/workspace",
		"WORKSPACE_KEYS_DIR=/etc/workspace/authorized_keys.d",
		"WORKSPACE_HOSTKEY_DIR=/etc/workspace/host_keys",
		"WORKSPACE_ENABLE_DIND=true",

		// One daemon, not one per account. A machine on somebody's own computer
		// has exactly one account, so a daemon each separates nobody from
		// anybody and ADR 0012's argument for the shared daemon applies in
		// full. It also keeps a fresh machine's first connection from waiting
		// on adoption, which asks dockerd before the agent serves.
		"WORKSPACE_PER_USER_DIND=false",
	}
	// Bound to every interface INSIDE the machine, not to its loopback.
	//
	// WSL2's default networking is NAT, and Windows reaches the machine either
	// through its localhost relay or at the machine's own address. A service on
	// the machine's loopback can only be reached by the first of those, and ADR
	// 0026 measured the relay not carrying it.
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
// Parsed here rather than by a shell pipeline in the command: `hostname -I`
// does not exist in busybox, and an awk field passed through two layers of
// quoting fails silently and returns the whole line.
func parseWSLAddress(raw []byte) string {
	fields := strings.Fields(decodeWSLOutput(raw))
	for i, f := range fields {
		if f == "inet" && i+1 < len(fields) {
			if addr := firstIPv4(fields[i+1 : i+2]); addr != "" {
				return addr
			}
		}
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
// an escaped quote, start again. What passes through here is our own config and
// a hex digest rather than anything hostile, so what it is for is a newline in
// the content not ending the command.
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
