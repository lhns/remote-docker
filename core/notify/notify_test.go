package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/workspace"
)

// The rejection table is the security-critical test in this package. Every row
// is a path that must never reach the agent's replay, because the agent turns
// one into a syscall as root.
func TestFSEventValidateRejects(t *testing.T) {
	const goodExport = "/m/0123456789abcdef"

	tests := []struct {
		name  string
		event Event
		want  string // substring of the error, so a rejection for the WRONG
		// reason still fails
	}{
		{"traversal", Event{goodExport, "/a/../../etc/shadow", OpWrite, false}, `".."`},
		{"bare traversal", Event{goodExport, "/..", OpWrite, false}, `".."`},
		{"dot component", Event{goodExport, "/a/./b", OpWrite, false}, `"."`},
		{"relative", Event{goodExport, "a/b", OpWrite, false}, "not absolute"},
		{"empty path", Event{goodExport, "", OpWrite, false}, "empty"},
		{"empty component", Event{goodExport, "/a//b", OpWrite, false}, "empty component"},
		{"backslash", Event{goodExport, `/a\b`, OpWrite, false}, "backslash"},
		{"windows spelling", Event{goodExport, `\a\b`, OpWrite, false}, "not absolute"},
		{"nul", Event{goodExport, "/a\x00b", OpWrite, false}, "NUL"},
		{"no op", Event{goodExport, "/a", 0, false}, "no operation"},
		{"unknown op", Event{goodExport, "/a", 1 << 7, false}, "unknown operation"},
		{"unknown op alongside known", Event{goodExport, "/a", OpWrite | 1<<6, false}, "unknown operation"},
		{"export not ours", Event{"/etc", "/a", OpWrite, false}, "export"},
		{"export empty", Event{"", "/a", OpWrite, false}, "export"},
		{"export absolute path", Event{"/m/../../etc", "/a", OpWrite, false}, "export"},
		{"export short id", Event{"/m/0123", "/a", OpWrite, false}, "export"},
		{"export non-hex id", Event{"/m/zzzzzzzzzzzzzzzz", "/a", OpWrite, false}, "export"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.event.Validate()
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tt.event)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate(%+v) = %q, want it to mention %q", tt.event, err, tt.want)
			}
		})
	}
}

func TestFSEventValidateAccepts(t *testing.T) {
	tests := []Event{
		{"/cwd", "/", OpWrite, true},
		{"/cwd", "/main.go", OpWrite, false},
		{"/m/0123456789abcdef", "/src/app/index.ts", OpCreate | OpWrite, false},
		{"/m/ffffffffffffffff", "/a b/c-d.e_f", OpAttrib, false},
		// A filename may legitimately contain dots, or start with one; only a
		// whole component of "." or ".." is a traversal.
		{"/cwd", "/.env", OpWrite, false},
		{"/cwd", "/..leading", OpWrite, false},
		{"/cwd", "/trailing..", OpWrite, false},
		{"/cwd", "/a...b", OpWrite, false},
		// Non-ASCII survives: macOS hands back NFD-decomposed names, and those
		// are the bytes our own NFS server will serve for that file.
		{"/cwd", "/café/résumé.txt", OpWrite, false},
		{"/cwd", "/日本語.txt", OpWrite, false},
		{"/cwd", "/deleted", OpRemove, false},
		{"/cwd", "/moved", OpRename, true},
	}

	for _, e := range tests {
		if err := e.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", e, err)
		}
	}
}

// The frame is the contract between two separately built binaries, so a change
// to a json tag has to fail here rather than in CI three phases later.
func TestNotifyFrameWireFormat(t *testing.T) {
	frame := Frame{
		Events: []Event{
			{Export: "/cwd", Path: "/main.go", Op: OpWrite},
			{Export: "/m/0123456789abcdef", Path: "/src", Op: OpCreate, Dir: true},
		},
	}

	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	const want = `{"v":[{"e":"/cwd","p":"/main.go","o":2},{"e":"/m/0123456789abcdef","p":"/src","o":1,"d":true}]}`
	if string(encoded) != want {
		t.Errorf("frame encoded as\n  %s\nwant\n  %s", encoded, want)
	}

	var back Frame
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Events) != 2 || back.Events[0] != frame.Events[0] || back.Events[1] != frame.Events[1] {
		t.Errorf("round trip = %+v, want %+v", back.Events, frame.Events)
	}
	if back.Hello != nil || back.Notice != nil {
		t.Error("absent payload fields decoded as non-nil")
	}
}

// A frame must be one line: the stream is newline-delimited, so an encoder
// that emitted a newline inside a frame would desynchronise both ends.
func TestNotifyFrameIsSingleLine(t *testing.T) {
	frame := Frame{
		Events: []Event{{Export: "/cwd", Path: "/a\nb", Op: OpWrite}},
		Notice: &Notice{Export: "/cwd", Path: "/", Reason: "overflow", Dropped: 12},
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "\n") {
		t.Errorf("encoded frame contains a newline: %s", encoded)
	}
}

func TestNotifyHelloWireFormat(t *testing.T) {
	encoded, err := json.Marshal(Frame{Hello: &Hello{Version: Version, Agent: "dev"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"h":{"v":1,"a":"dev"}}`
	if string(encoded) != want {
		t.Errorf("hello encoded as %s, want %s", encoded, want)
	}
}

func TestFSOpString(t *testing.T) {
	tests := []struct {
		op   Op
		want string
	}{
		{0, "none"},
		{OpWrite, "write"},
		{OpCreate | OpWrite, "create|write"},
		{OpRemove, "remove"},
		{OpCreate | OpWrite | OpRemove | OpRename | OpAttrib, "create|write|remove|rename|attrib"},
		{1 << 7, "unknown(0x80)"},
		{OpWrite | 1<<7, "write|unknown(0x80)"},
	}
	for _, tt := range tests {
		if got := tt.op.String(); got != tt.want {
			t.Errorf("Op(%#x).String() = %q, want %q", uint8(tt.op), got, tt.want)
		}
	}
}

// The export accepted by an event must be exactly the set the agent can turn
// into a volume, or the client can send something well-formed that the agent
// then refuses, a disagreement that would present as "notifications
// mysteriously do not work for this one share".
func TestValidateExportMatchesVolumeResolution(t *testing.T) {
	for _, export := range []string{"/cwd", "/m/0123456789abcdef", "/m/zzz", "/etc", "", "/m/"} {
		volErr := workspace.ValidExport(export)
		evErr := Event{Export: export, Path: "/a", Op: OpWrite}.Validate()
		if (volErr == nil) != (evErr == nil) {
			t.Errorf("export %q: workspace.ValidExport err=%v but Validate err=%v", export, volErr, evErr)
		}
	}
}
