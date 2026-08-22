package workspace

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// This is verbatim what image/bin/workspace-info prints today. The Go client
// is built and proven against the existing sshd-based server before the Go
// agent replaces it, so if this test ever needs changing, the agent has
// stopped being a drop-in substitution and the sequencing is broken.
const shellScriptOutput = `WORKSPACE_USER=alice
WORKSPACE_UID=10000
WORKSPACE_GID=10000
WORKSPACE_NFS_PORT=30000
WORKSPACE_MOUNTPOINT=/home/alice/workspace
WORKSPACE_MOUNTED=false
WORKSPACE_DOCKER=28.0.1
`

func TestParseInfoAcceptsShellScriptOutput(t *testing.T) {
	got, err := ParseInfo(strings.NewReader(shellScriptOutput))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	// The two mount keys are what an OLD agent still sends: they described the
	// `~/workspace` convenience mount that ADR 0018 deleted. They must land in
	// Extra rather than being rejected, which is the whole point of Extra.
	want := Info{
		User:    "alice",
		UID:     10000,
		GID:     10000,
		NFSPort: 30000,
		Docker:  "28.0.1",
		Extra: map[string]string{
			"WORKSPACE_MOUNTPOINT": "/home/alice/workspace",
			"WORKSPACE_MOUNTED":    "false",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseInfo() = %+v, want %+v", got, want)
	}
}

// The port the client reads here is the port it tunnels to, and it must equal
// the port the server derived from the uid. Asserting it against the shared
// mapping is what stops the two sides drifting.
func TestParsedPortAgreesWithMapping(t *testing.T) {
	info, err := ParseInfo(strings.NewReader(shellScriptOutput))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	want, err := DefaultMapping().PortForUID(info.UID)
	if err != nil {
		t.Fatalf("PortForUID(%d): %v", info.UID, err)
	}
	if info.NFSPort != want {
		t.Errorf("reported port %d, but uid %d maps to %d", info.NFSPort, info.UID, want)
	}
}

func TestParseInfoDockerUnavailable(t *testing.T) {
	in := "WORKSPACE_USER=bob\nWORKSPACE_NFS_PORT=30001\nWORKSPACE_DOCKER=unavailable\n"
	got, err := ParseInfo(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if got.Docker != DockerUnavailable {
		t.Errorf("Docker = %q, want %q", got.Docker, DockerUnavailable)
	}
}

// An older client must stay usable against a newer server, so an unrecognised
// key is data to carry, not an error.
func TestParseInfoKeepsUnknownKeys(t *testing.T) {
	in := shellScriptOutput + "WORKSPACE_FUTURE_THING=42\n"
	got, err := ParseInfo(strings.NewReader(in))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if got.Extra["WORKSPACE_FUTURE_THING"] != "42" {
		t.Errorf("Extra = %v, want WORKSPACE_FUTURE_THING=42", got.Extra)
	}
}

func TestParseInfoIgnoresBlankAndComment(t *testing.T) {
	in := "\n# a comment\n" + shellScriptOutput + "\n"
	if _, err := ParseInfo(strings.NewReader(in)); err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
}

func TestParseInfoRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no port", "WORKSPACE_USER=alice\n"},
		{"no user", "WORKSPACE_NFS_PORT=30000\n"},
		{"empty user", "WORKSPACE_USER=\nWORKSPACE_NFS_PORT=30000\n"},
		{"port zero", "WORKSPACE_USER=alice\nWORKSPACE_NFS_PORT=0\n"},
		{"port above maximum", "WORKSPACE_USER=alice\nWORKSPACE_NFS_PORT=70000\n"},
		{"non-numeric port", "WORKSPACE_USER=alice\nWORKSPACE_NFS_PORT=abc\n"},
		{"non-numeric uid", "WORKSPACE_USER=alice\nWORKSPACE_UID=x\nWORKSPACE_NFS_PORT=30000\n"},
		{"line without a separator", "WORKSPACE_USER=alice\nnonsense\nWORKSPACE_NFS_PORT=30000\n"},
		{"empty input", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseInfo(strings.NewReader(tt.in)); err == nil {
				t.Errorf("ParseInfo(%q) = nil error, want an error", tt.in)
			}
		})
	}
}

func TestInfoRoundTrip(t *testing.T) {
	want := Info{
		User:    "alice",
		UID:     10000,
		GID:     10000,
		NFSPort: 30000,
		Docker:  "28.0.1",
		Extra:   map[string]string{"WORKSPACE_ZZZ": "z", "WORKSPACE_AAA": "a"},
	}

	var buf strings.Builder
	if err := want.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := ParseInfo(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestEncodeIsDeterministic(t *testing.T) {
	info := Info{
		User: "alice", NFSPort: 30000,
		Extra: map[string]string{"B": "2", "A": "1", "C": "3"},
	}
	var first strings.Builder
	if err := info.Encode(&first); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for range 20 {
		var next strings.Builder
		if err := info.Encode(&next); err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if next.String() != first.String() {
			t.Fatalf("Encode is not deterministic:\n%s\nvs\n%s", first.String(), next.String())
		}
	}
}

func TestEncodeDefaultsDockerToUnavailable(t *testing.T) {
	var buf strings.Builder
	if err := (Info{User: "alice", NFSPort: 30000}).Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(buf.String(), keyDocker+"="+DockerUnavailable) {
		t.Errorf("Encode() = %q, want it to report docker as %q", buf.String(), DockerUnavailable)
	}
}

// The agent's version was added after the format was in use, which is only
// safe because unrecognised keys survive. An old client reading a new agent's
// reply must ignore it rather than fail, and a new client reading an old
// agent's must see it empty rather than invent one.
func TestAgentVersionIsBackwardCompatible(t *testing.T) {
	// An old agent's reply: no WORKSPACE_AGENT at all.
	old := "WORKSPACE_USER=alice\nWORKSPACE_NFS_PORT=30001\nWORKSPACE_DOCKER=28.5.2\n"
	info, err := ParseInfo(strings.NewReader(old))
	if err != nil {
		t.Fatalf("parsing an old agent's reply: %v", err)
	}
	if info.Agent != "" {
		t.Errorf("Agent = %q from a reply that had none", info.Agent)
	}
	if info.User != "alice" || info.NFSPort != 30001 {
		t.Errorf("an old reply did not parse: %+v", info)
	}

	// A new agent's reply round trips.
	var buf bytes.Buffer
	want := Info{User: "alice", NFSPort: 30001, Docker: "28.5.2", Agent: "sha-abc1234"}
	if err := want.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := ParseInfo(&buf)
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	if got.Agent != "sha-abc1234" {
		t.Errorf("Agent = %q, want the agent's build", got.Agent)
	}
}
