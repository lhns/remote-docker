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

// generationFile is where a machine's generation is kept, INSIDE the
// distribution: one exported, moved or deleted by hand carries it or takes it
// with it. A file beside the config can disagree with reality, and a generation
// that lies is worse than one that is missing, because Plan trusts a mismatch
// enough to recreate.
const generationFile = "/etc/remote-docker-generation"

// agentLog is where the agent's output goes inside the machine. WSL's boot
// command has no console, so without this an agent that refuses to start is a
// machine that is simply unreachable, with the reason written to a closed file
// descriptor.
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

// wslConf is the distribution's own configuration. `[boot] command` starts the
// agent every time WSL starts the distribution, so nothing on the Windows side
// supervises anything; systemd is off because the agent supervises dockerd
// itself (ADR 0010).
func wslConf(spec Spec) string {
	env := []string{
		// The image's environment, which a rootfs does not carry (CLAUDE.md, "a
		// rootfs is a filesystem"). DOCKER_TLS_CERTDIR is EMPTY, not unset:
		// dind's docker-entrypoint.sh reads the difference.
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"DOCKER_TLS_CERTDIR=",

		"WORKSPACE_STATE_DIR=/etc/workspace",
		"WORKSPACE_KEYS_DIR=/etc/workspace/authorized_keys.d",
		"WORKSPACE_HOSTKEY_DIR=/etc/workspace/host_keys",
		"WORKSPACE_ENABLE_DIND=true",

		// One daemon: a machine on somebody's own computer has one account, so
		// ADR 0012's argument applies in full, and a first connection does not
		// wait on adoption.
		"WORKSPACE_PER_USER_DIND=false",
	}
	// Every interface inside the machine, not its loopback: Windows reaches it
	// at the machine's own address, the localhost relay having been measured
	// not to carry it (ADR 0026). That interface is host-only, and the agent
	// authenticates every connection by key.
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

// wslWriteArgs writes a file inside a distribution, with printf: one process
// and nothing that differs between the shells a rootfs might ship.
func wslWriteArgs(name, path, content string) []string {
	return wslRunArgs(name, "sh", "-c", "printf '%s' "+shellQuote(content)+" > "+path)
}

// shellQuote wraps a string in single quotes for `sh -c`, so a newline in the
// content does not end the command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// wslImportArgs is the command that creates a distribution from a rootfs.
func wslImportArgs(name, dir, rootfs string) []string {
	return []string{"--import", name, dir, rootfs, "--version", "2"}
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
