package rewrite

import (
	"encoding/json"
	"testing"

	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/core/workspace"
)

var rule = ports.Rule{User: "alice", ClientID: "me"}

func labels(owner, client, asked string) string {
	m := map[string]string{workspace.OwnerLabel: owner}
	if client != "" {
		m[workspace.ClientLabel] = client
	}
	if asked != "" {
		m[workspace.PortsLabel] = asked
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// shown decodes a rewritten list into "name: ip:port->private/type" lines so a
// table can say what a user sees.
func shownList(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	var list []struct {
		Names []string
		Ports []struct {
			IP          string
			PrivatePort int
			PublicPort  int
			Type        string
		}
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("rewritten list is not JSON: %v\n%s", err, body)
	}
	out := map[string][]string{}
	for _, c := range list {
		lines := []string{}
		for _, p := range c.Ports {
			lines = append(lines, p.IP+":"+itoa(p.PublicPort)+"->"+itoa(p.PrivatePort)+"/"+p.Type)
		}
		out[c.Names[0]] = lines
	}
	return out
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListPortsShowsThePortAskedFor(t *testing.T) {
	body := `[
	 {"Names":["/asked"],"Unknown":{"keep":"me"},"Labels":` + labels("alice", "me", "80/tcp=8080") + `,"Ports":[
	   {"IP":"0.0.0.0","PrivatePort":80,"PublicPort":32790,"Type":"tcp"},
	   {"IP":"::","PrivatePort":80,"PublicPort":32790,"Type":"tcp"}]},
	 {"Names":["/twice"],"Labels":` + labels("alice", "me", "80/tcp=8080;9090") + `,"Ports":[
	   {"IP":"0.0.0.0","PrivatePort":80,"PublicPort":32791,"Type":"tcp"},
	   {"IP":"::","PrivatePort":80,"PublicPort":32791,"Type":"tcp"}]},
	 {"Names":["/random"],"Labels":` + labels("alice", "me", "") + `,"Ports":[
	   {"IP":"0.0.0.0","PrivatePort":80,"PublicPort":32792,"Type":"tcp"},
	   {"IP":"::","PrivatePort":80,"PublicPort":32792,"Type":"tcp"},
	   {"PrivatePort":9000,"Type":"tcp"}]},
	 {"Names":["/other-machine"],"Labels":` + labels("alice", "them", "80/tcp=8080") + `,"Ports":[
	   {"IP":"0.0.0.0","PrivatePort":80,"PublicPort":32793,"Type":"tcp"}]},
	 {"Names":["/other-account"],"Labels":` + labels("bob", "them", "80/tcp=8080") + `,"Ports":[
	   {"IP":"0.0.0.0","PrivatePort":80,"PublicPort":32794,"Type":"tcp"}]},
	 {"Names":["/udp"],"Labels":` + labels("alice", "me", "53/udp=5353") + `,"Ports":[
	   {"IP":"0.0.0.0","PrivatePort":53,"PublicPort":32795,"Type":"udp"},
	   {"IP":"0.0.0.0","PrivatePort":53,"PublicPort":32796,"Type":"tcp"}]},
	 {"Names":["/none"],"Labels":` + labels("alice", "me", "") + `,"Ports":[]}
	]`

	got, ok := ListPorts([]byte(body), rule)
	if !ok {
		t.Fatal("ListPorts left the body alone")
	}
	want := map[string][]string{
		"/asked":         {"127.0.0.1:8080->80/tcp"},
		"/twice":         {"127.0.0.1:8080->80/tcp", "127.0.0.1:9090->80/tcp"},
		"/random":        {"127.0.0.1:32792->80/tcp", ":0->9000/tcp"},
		"/other-machine": {"0.0.0.0:32793->80/tcp"},
		"/other-account": {"0.0.0.0:32794->80/tcp"},
		"/udp":           {"127.0.0.1:5353->53/udp", "127.0.0.1:32796->53/tcp"},
		"/none":          {},
	}
	// Another machine's container is still ours to forward, on the published
	// number, so it is shown on loopback too.
	want["/other-machine"] = []string{"127.0.0.1:32793->80/tcp"}
	// Another account's is not forwarded here, so nothing about it changes.
	for name, lines := range shownList(t, got) {
		if !equal(lines, want[name]) {
			t.Errorf("%s shows %v, want %v", name, lines, want[name])
		}
	}

	var list []map[string]json.RawMessage
	if err := json.Unmarshal(got, &list); err != nil {
		t.Fatal(err)
	}
	if string(list[0]["Unknown"]) != `{"keep":"me"}` {
		t.Errorf("an unknown field became %s", list[0]["Unknown"])
	}
}

func TestListPortsLeavesAnythingElseAlone(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":              `not json`,
		"an object":             `{"Ports":[]}`,
		"no containers":         `[]`,
		"nobody's ports":        `[{"Labels":` + labels("alice", "me", "") + `,"Ports":[{"PrivatePort":80,"Type":"tcp"}]}]`,
		"another account's":     `[{"Labels":` + labels("bob", "me", "80/tcp=8080") + `,"Ports":[{"IP":"0.0.0.0","PrivatePort":80,"PublicPort":32790,"Type":"tcp"}]}]`,
		"ports that are a mess": `[{"Labels":` + labels("alice", "me", "") + `,"Ports":"x"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := ListPorts([]byte(body), rule); ok {
				t.Errorf("rewrote %s into %s", body, got)
			}
		})
	}
}

func inspectBody(client, asked string) string {
	return `{"Id":"abc","Extra":[1,2],"Config":{"Labels":` + labels("alice", client, asked) + `},
	"HostConfig":{"NetworkMode":"bridge","PortBindings":{
	  "80/tcp":[{"HostIp":"127.0.0.1","HostPort":""}],
	  "443/tcp":[{"HostIp":"","HostPort":""}]}},
	"NetworkSettings":{"Bridge":"","Ports":{
	  "80/tcp":[{"HostIp":"0.0.0.0","HostPort":"32790"},{"HostIp":"::","HostPort":"32790"}],
	  "443/tcp":[{"HostIp":"0.0.0.0","HostPort":"32791"}],
	  "9000/tcp":null}}}`
}

func decode(t *testing.T, body []byte, path ...string) json.RawMessage {
	t.Helper()
	cur := json.RawMessage(body)
	for _, key := range path {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(cur, &obj); err != nil {
			t.Fatalf("%s: %v in %s", key, err, cur)
		}
		cur = obj[key]
	}
	return cur
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	b, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInspectPortsShowsAndRestoresWhatWasAsked(t *testing.T) {
	got, ok := InspectPorts([]byte(inspectBody("me", "80/tcp=8080;9090")), rule)
	if !ok {
		t.Fatal("InspectPorts left the body alone")
	}

	shown := compact(t, decode(t, got, "NetworkSettings", "Ports"))
	wantShown := `{"443/tcp":[{"HostIp":"127.0.0.1","HostPort":"32791"}],` +
		`"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"},{"HostIp":"127.0.0.1","HostPort":"9090"}],` +
		`"9000/tcp":null}`
	if shown != wantShown {
		t.Errorf("NetworkSettings.Ports =\n%s\nwant\n%s", shown, wantShown)
	}

	asked := compact(t, decode(t, got, "HostConfig", "PortBindings"))
	wantAsked := `{"443/tcp":[{"HostIp":"","HostPort":""}],` +
		`"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"},{"HostIp":"127.0.0.1","HostPort":"9090"}]}`
	if asked != wantAsked {
		t.Errorf("HostConfig.PortBindings =\n%s\nwant\n%s", asked, wantAsked)
	}

	if extra := compact(t, decode(t, got, "Extra")); extra != "[1,2]" {
		t.Errorf("an unknown field became %s", extra)
	}
	if mode := compact(t, decode(t, got, "HostConfig", "NetworkMode")); mode != `"bridge"` {
		t.Errorf("a sibling of PortBindings became %s", mode)
	}
}

// Another machine's container: shown where it is forwarded, and what its
// creator asked for stays its creator's.
func TestInspectPortsKeepsAnotherMachinesRequest(t *testing.T) {
	got, ok := InspectPorts([]byte(inspectBody("them", "80/tcp=8080")), rule)
	if !ok {
		t.Fatal("InspectPorts left the body alone")
	}
	shown := compact(t, decode(t, got, "NetworkSettings", "Ports"))
	wantShown := `{"443/tcp":[{"HostIp":"127.0.0.1","HostPort":"32791"}],` +
		`"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"32790"}],"9000/tcp":null}`
	if shown != wantShown {
		t.Errorf("NetworkSettings.Ports =\n%s\nwant\n%s", shown, wantShown)
	}
	if asked := compact(t, decode(t, got, "HostConfig", "PortBindings")); asked != `{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":""}],"443/tcp":[{"HostIp":"","HostPort":""}]}` {
		t.Errorf("HostConfig.PortBindings changed to %s", asked)
	}
}

func TestInspectPortsLeavesAnythingElseAlone(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":        `nope`,
		"an array":        `[]`,
		"another account": `{"Config":{"Labels":` + labels("bob", "me", "80/tcp=8080") + `},"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"32790"}]}}}`,
		"no labels":       `{"NetworkSettings":{"Ports":{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"32790"}]}}}`,
		"nothing bound":   `{"Config":{"Labels":` + labels("alice", "me", "") + `},"NetworkSettings":{"Ports":{"80/tcp":null}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := InspectPorts([]byte(body), rule); ok {
				t.Errorf("rewrote %s into %s", body, got)
			}
		})
	}
}
