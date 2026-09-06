package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	want := File{
		Workspaces: map[string]Workspace{
			"dev": {Host: "dev.example", Port: 2222, User: "alice", Watch: "partial"},
		},
		Default: "dev",
	}
	if err := Save(want, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Default != "dev" || !reflect.DeepEqual(got.Workspaces, want.Workspaces) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// Save replaces the file rather than truncating it in place, because this is
// the only record of how to reach a workspace and half of it is worse than
// none.
func TestSaveDoesNotDestroyOnRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := Save(File{Default: "a", Workspaces: map[string]Workspace{"a": {Host: "a"}}}, path); err != nil {
		t.Fatal(err)
	}
	if err := Save(File{Default: "b", Workspaces: map[string]Workspace{"b": {Host: "b"}}}, path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Default != "b" || len(got.Workspaces) != 1 {
		t.Errorf("second save left %+v", got)
	}
	// No stray temporary files.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the config", names)
	}
}

// A file written by hand often describes one workspace with no name. Adding a
// second has to move the first into the keyed form, or the top-level fields
// would shadow it and it would silently stop being reachable.
func TestSetMigratesTheFlatForm(t *testing.T) {
	f := File{Workspace: Workspace{Host: "old.example", Port: 2200, User: "bob"}}
	if err := f.Set("new", Workspace{Host: "new.example", Port: 2222, User: "alice"}); err != nil {
		t.Fatal(err)
	}
	if f.Host != "" {
		t.Error("the flat form was left in place and will shadow the keyed one")
	}
	if len(f.Workspaces) != 2 {
		t.Fatalf("kept %d workspaces, want 2: %+v", len(f.Workspaces), f.Workspaces)
	}
	moved, ok := f.Workspaces["old.example"]
	if !ok || moved.Host != "old.example" || moved.User != "bob" || moved.Port != 2200 {
		t.Errorf("the existing workspace was not preserved: %+v", f.Workspaces)
	}
}

// Everything the flat entry said moves with it. Left at the top level, those
// fields are a base under every keyed entry (applyWorkspace runs twice), so the
// workspace being added inherits them -- and `machine` is the one that hurts:
// the new workspace would claim a Linux system belonging to the old one, which
// `rm` would then destroy.
func TestSetMovesTheWholeFlatEntry(t *testing.T) {
	f := File{Workspace: Workspace{
		Host:    "old.example",
		User:    "bob",
		Watch:   "partial",
		Machine: &Machine{Backend: "wsl", Name: "old"},
	}}
	if err := f.Set("new", Workspace{Host: "new.example", User: "alice"}); err != nil {
		t.Fatal(err)
	}

	if f.Machine != nil {
		t.Error("the flat entry's machine stayed at the top level, where every workspace inherits it")
	}
	if f.Watch != "" {
		t.Errorf("the flat entry's watch mode stayed at the top level as %q", f.Watch)
	}
	moved := f.Workspaces["old.example"]
	if moved.Machine == nil || moved.Machine.Name != "old" {
		t.Errorf("the machine did not move with its workspace: %+v", moved)
	}
	if moved.Watch != "partial" {
		t.Errorf("the watch mode did not move with its workspace: %+v", moved)
	}
}

func TestSetIsIdempotentAndUpdates(t *testing.T) {
	var f File
	_ = f.Set("dev", Workspace{Host: "a"})
	_ = f.Set("dev", Workspace{Host: "b"})
	if len(f.Workspaces) != 1 || f.Workspaces["dev"].Host != "b" {
		t.Errorf("re-adding did not update in place: %+v", f.Workspaces)
	}
	if f.Default != "dev" {
		t.Errorf("first workspace did not become the default")
	}
}

func TestSetRequiresAName(t *testing.T) {
	var f File
	if err := f.Set("", Workspace{Host: "a"}); err == nil {
		t.Error("an unnamed workspace was accepted")
	}
}

func TestRemove(t *testing.T) {
	f := File{
		Workspaces: map[string]Workspace{"a": {Host: "a"}, "b": {Host: "b"}},
		Default:    "a",
	}
	if !f.Remove("a") {
		t.Fatal("Remove reported nothing to remove")
	}
	// Exactly one left, so promoting it is unambiguous.
	if f.Default != "b" {
		t.Errorf("default = %q, want the only remaining workspace", f.Default)
	}
	if f.Remove("gone") {
		t.Error("Remove claimed to remove something that was not there")
	}
}

// With several left, picking a new default would be choosing for the user.
func TestRemoveLeavesTheDefaultUnsetWhenAmbiguous(t *testing.T) {
	f := File{
		Workspaces: map[string]Workspace{"a": {}, "b": {}, "c": {}},
		Default:    "a",
	}
	f.Remove("a")
	if f.Default != "" {
		t.Errorf("default = %q, want it left unset with two candidates", f.Default)
	}
}

func TestRemoveKeepsAnUnrelatedDefault(t *testing.T) {
	f := File{Workspaces: map[string]Workspace{"a": {}, "b": {}}, Default: "b"}
	f.Remove("a")
	if f.Default != "b" {
		t.Errorf("default = %q, want it untouched", f.Default)
	}
}

func TestKeyCommentIdentifiesTheMachine(t *testing.T) {
	got := KeyComment()
	if got == "" || got == "remote-docker" {
		t.Errorf("KeyComment() = %q, which identifies nothing", got)
	}
}

// The flat form must survive Workspace being EMBEDDED in File rather than
// repeated field for field.
//
// encoding/json inlines an anonymous embedded struct's fields, tags and all,
// so the shape is unchanged, but that is a property of the encoder rather
// than of this code, and the config file is somebody's file on disk. Asserted
// on the bytes, in both directions, so a future change to the struct that
// nests them instead fails here rather than in a user's home directory.
func TestTheFlatFormKeepsItsJSONShape(t *testing.T) {
	const flat = `{"host":"dev.example","port":2200,"user":"alice","watch":"partial"}`

	var f File
	if err := json.Unmarshal([]byte(flat), &f); err != nil {
		t.Fatalf("parsing the flat form: %v", err)
	}
	if f.Host != "dev.example" || f.Port != 2200 || f.User != "alice" || f.Watch != "partial" {
		t.Fatalf("the flat form did not reach the fields: %+v", f)
	}

	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}
	if string(out) != flat {
		t.Errorf("the flat form re-marshalled as\n  %s\nwant\n  %s", out, flat)
	}
}

// And the keyed form, for the same reason: `workspaces` and `default` sit
// beside the inlined fields rather than under them.
func TestTheKeyedFormKeepsItsJSONShape(t *testing.T) {
	const keyed = `{"workspaces":{"dev":{"host":"dev.example"}},"default":"dev"}`

	var f File
	if err := json.Unmarshal([]byte(keyed), &f); err != nil {
		t.Fatalf("parsing the keyed form: %v", err)
	}
	out, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}
	if string(out) != keyed {
		t.Errorf("the keyed form re-marshalled as\n  %s\nwant\n  %s", out, keyed)
	}
}
