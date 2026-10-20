package rewrite

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/lhns/remote-docker/client/internal/ports"
	"github.com/lhns/remote-docker/core/workspace"
)

// What docker ps, port and inspect show for a published port (ADR 0008): the
// number opened on this machine, which is what the user asked for, instead of
// the one the workspace's daemon picked. Both functions take the session's
// ports.Rule, so the display and the forwards come from one decision.
//
// Bodies stay generic JSON and only the port fields are touched. The second
// result is false when nothing changed or the body is not what it should be,
// and the caller then relays the daemon's bytes as they came.

// localHost is what the forwards bind (ports.bindAddr), so what is shown.
const localHost = "127.0.0.1"

// ListPorts rewrites the Ports of each container in a GET /containers/json
// body.
func ListPorts(body []byte, rule ports.Rule) ([]byte, bool) {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, false
	}

	changed := false
	for _, entry := range list {
		var labels map[string]string
		_ = json.Unmarshal(entry["Labels"], &labels)

		var shown []map[string]json.RawMessage
		if err := json.Unmarshal(entry["Ports"], &shown); err != nil || len(shown) == 0 {
			continue
		}
		c := ports.Container{Labels: labels}
		if !rule.Owned(c) {
			continue
		}

		if !slices.ContainsFunc(shown, func(p map[string]json.RawMessage) bool {
			return string(p["PublicPort"]) != "" && string(p["PublicPort"]) != "0"
		}) {
			continue
		}

		out := make([]map[string]json.RawMessage, 0, len(shown))
		seen := map[string]bool{}
		for _, p := range shown {
			var public, private int
			var proto string
			_ = json.Unmarshal(p["PublicPort"], &public)
			_ = json.Unmarshal(p["PrivatePort"], &private)
			_ = json.Unmarshal(p["Type"], &proto)
			if public == 0 {
				out = append(out, p)
				continue
			}
			// One entry per address family from the daemon; one per publication here.
			published := ports.Published{PublicPort: public, PrivatePort: private, Type: proto}
			key := published.Network() + "/" + strconv.Itoa(public) + "/" + strconv.Itoa(private)
			if seen[key] {
				continue
			}
			seen[key] = true

			for _, local := range rule.Local(c, published) {
				shownPort := maps.Clone(p)
				shownPort["IP"] = json.RawMessage(strconv.Quote(localHost))
				shownPort["PublicPort"] = json.RawMessage(strconv.Itoa(local))
				out = append(out, shownPort)
			}
		}

		encoded, err := encode(out)
		if err != nil {
			return nil, false
		}
		entry["Ports"] = encoded
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := encode(list)
	return out, err == nil
}

// InspectPorts rewrites NetworkSettings.Ports of a GET /containers/{id}/json
// body to the local numbers, and HostConfig.PortBindings back to what was
// asked, since the create rewrite emptied it.
func InspectPorts(body []byte, rule ports.Rule) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}

	var config struct {
		Labels map[string]string
	}
	_ = json.Unmarshal(doc["Config"], &config)
	c := ports.Container{Labels: config.Labels}
	if !rule.Owned(c) {
		return nil, false
	}

	changed := false
	if settings, ok := decodeObject(doc["NetworkSettings"]); ok {
		if shown, ok := localBindings(settings["Ports"], c, rule); ok {
			settings["Ports"] = shown
			if encoded, err := encode(settings); err == nil {
				doc["NetworkSettings"] = encoded
				changed = true
			}
		}
	}
	if hostConfig, ok := decodeObject(doc["HostConfig"]); ok {
		if asked, ok := askedBindings(hostConfig["PortBindings"], c, rule); ok {
			hostConfig["PortBindings"] = asked
			if encoded, err := encode(hostConfig); err == nil {
				doc["HostConfig"] = encoded
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	out, err := encode(doc)
	return out, err == nil
}

// localBindings maps each "80/tcp" of NetworkSettings.Ports to the local
// numbers, one entry per number.
func localBindings(raw json.RawMessage, c ports.Container, rule ports.Rule) (json.RawMessage, bool) {
	var bindings map[string][]binding
	if err := json.Unmarshal(raw, &bindings); err != nil {
		return nil, false
	}

	changed := false
	for containerPort, list := range bindings {
		private, err := strconv.Atoi(strings.SplitN(containerPort, "/", 2)[0])
		if err != nil {
			continue
		}
		proto := workspace.ProtoOf(containerPort)

		var out []binding
		seen := map[int]bool{}
		for _, b := range list {
			var hostPort string
			public, err := 0, json.Unmarshal(b["HostPort"], &hostPort)
			if err == nil {
				public, err = strconv.Atoi(hostPort)
			}
			if err != nil || public == 0 {
				out = append(out, b)
				continue
			}
			if seen[public] {
				continue
			}
			seen[public] = true

			for _, local := range rule.Local(c, ports.Published{PublicPort: public, PrivatePort: private, Type: proto}) {
				shown := maps.Clone(b)
				shown["HostIp"] = json.RawMessage(strconv.Quote(localHost))
				shown["HostPort"] = json.RawMessage(strconv.Quote(strconv.Itoa(local)))
				out = append(out, shown)
			}
			changed = true
		}
		bindings[containerPort] = out
	}
	if !changed {
		return nil, false
	}
	encoded, err := encode(bindings)
	return encoded, err == nil
}

// askedBindings is HostConfig.PortBindings as the user wrote it: the requested
// numbers where this machine asked for any, the daemon's entry otherwise.
func askedBindings(raw json.RawMessage, c ports.Container, rule ports.Rule) (json.RawMessage, bool) {
	var bindings map[string][]binding
	if err := json.Unmarshal(raw, &bindings); err != nil || len(bindings) == 0 {
		return nil, false
	}

	changed := false
	for containerPort, list := range bindings {
		asked := rule.LocalPorts(c, portOf(containerPort))
		if len(asked) == 0 || len(list) == 0 {
			continue
		}
		var out []binding
		for _, port := range asked {
			b := maps.Clone(list[0])
			b["HostPort"] = json.RawMessage(strconv.Quote(strconv.Itoa(port)))
			out = append(out, b)
		}
		bindings[containerPort] = out
		changed = true
	}
	if !changed {
		return nil, false
	}
	encoded, err := encode(bindings)
	return encoded, err == nil
}

// portOf is the Published a PortBindings key stands for, enough to ask the
// rule about it.
func portOf(containerPort string) ports.Published {
	number, _, _ := strings.Cut(containerPort, "/")
	private, _ := strconv.Atoi(number)
	return ports.Published{PrivatePort: private, Type: workspace.ProtoOf(containerPort)}
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// encode is json.Marshal without escaping <, > and &, so text the daemon sent
// comes back as it was written.
func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
