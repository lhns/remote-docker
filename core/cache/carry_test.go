package cache

import (
	"encoding/json"
	"strings"
	"testing"
)

// The write-back additions are all optional fields, absent unless asked for and
// announced (ADR 0044). An old client must read a new agent's replies as it
// always did, and a new client must read an old agent's.

// A change that is not a replaced directory or a link encodes exactly as it did,
// so an old client sees nothing new.
func TestAnOrdinaryChangeEncodesAsItAlwaysDid(t *testing.T) {
	got, err := json.Marshal(Change{Path: "/a.go", Size: 3, ModTime: 9})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"p":"/a.go","s":3,"m":9}`; string(got) != want {
		t.Errorf("a change encodes as %s, want %s", got, want)
	}
	got, _ = json.Marshal(Change{Path: "/gone", Deleted: true})
	if want := `{"p":"/gone","d":true}`; string(got) != want {
		t.Errorf("a deletion encodes as %s, want %s", got, want)
	}
}

// What an agent from before Carry sent still decodes, with nothing carried.
func TestAnOlderAgentsGreetingAndReplyDecodeWithNothingCarried(t *testing.T) {
	var greeting Reply
	if err := json.Unmarshal([]byte(`{"hello":{"v":1,"z":["zstd"],"o":true}}`), &greeting); err != nil {
		t.Fatal(err)
	}
	if greeting.Hello == nil || greeting.Hello.Carry {
		t.Errorf("an older agent's greeting decodes as %+v, want Carry false", greeting.Hello)
	}

	var reply Reply
	if err := json.Unmarshal([]byte(`{"c":[{"p":"/a.go","s":3,"m":9}]}`), &reply); err != nil {
		t.Fatal(err)
	}
	if ch := reply.Changes[0]; ch.Opaque || ch.Link != "" || reply.Skipped != nil {
		t.Errorf("an older agent's reply decodes as %+v / %v, want nothing carried", ch, reply.Skipped)
	}
}

// The new fields round-trip, and a request that does not ask carries no field
// an older agent would have to ignore.
func TestTheCarryFieldsRoundTrip(t *testing.T) {
	data, err := json.Marshal(Reply{
		Hello:   &Hello{Version: Version, Carry: true},
		Changes: []Change{{Path: "/tools", Opaque: true}, {Path: "/bin/tool", Link: "../x"}},
		Skipped: []string{"/b.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back Reply
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Hello.Carry || !back.Changes[0].Opaque || back.Changes[1].Link != "../x" || back.Skipped[0] != "/b.go" {
		t.Errorf("round trip lost a field: %s -> %+v", data, back)
	}

	plain, _ := json.Marshal(Request{Op: OpChanges, Export: "/m/00112233445566ff"})
	if strings.Contains(string(plain), `"y"`) {
		t.Errorf("a request that did not ask carries the field anyway: %s", plain)
	}
	asked, _ := json.Marshal(Request{Op: OpChanges, Export: "/m/00112233445566ff", Carry: true})
	if !strings.Contains(string(asked), `"y":true`) {
		t.Errorf("a request that asked does not say so: %s", asked)
	}
	if err := (Request{Op: OpChanges, Export: "/m/00112233445566ff", Carry: true}).Validate(); err != nil {
		t.Errorf("a request asking for Carry is refused: %v", err)
	}
}

// The times are negotiated like Carry: an older agent's greeting announces
// none, a request that did not ask names none, and what a new agent sends
// round-trips (ADR 0044).
func TestTheTimesFieldsAreNegotiatedAndRoundTrip(t *testing.T) {
	var greeting Reply
	if err := json.Unmarshal([]byte(`{"hello":{"v":1,"z":["zstd"],"o":true,"w":true}}`), &greeting); err != nil {
		t.Fatal(err)
	}
	if greeting.Hello.Times {
		t.Errorf("an older agent's greeting decodes with Times set: %+v", greeting.Hello)
	}

	plain, _ := json.Marshal(Request{Op: OpChanges, Export: "/m/00112233445566ff", Carry: true})
	if strings.Contains(string(plain), `"t"`) {
		t.Errorf("a request that did not ask for times names them: %s", plain)
	}

	data, err := json.Marshal(Reply{
		Hello:   &Hello{Version: Version, Times: true},
		Changes: []Change{{Path: "/a", Size: 1, ModTime: 2, Changed: 3, Mode: 0o755}, {Path: "/d", Dir: true, Changed: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var back Reply
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Hello.Times || back.Changes[0].Changed != 3 || back.Changes[0].Mode != 0o755 ||
		!back.Changes[1].Dir || back.Changes[1].Changed != 4 {
		t.Errorf("round trip lost a field: %s -> %+v", data, back)
	}
}
