package main

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/client/internal/config"
)

func withHostPorts(t *testing.T, list string) {
	t.Helper()
	t.Setenv(config.EnvHostPorts, "")
	withConfig(t, &config.File{Default: "dev", Workspaces: map[string]config.Workspace{
		"dev": {Host: "dev.example", HostPorts: list},
		"ci":  {Host: "ci.example", HostPorts: "9000"},
	}})
}

func TestHostPortsAddAndRemoveAreIdempotent(t *testing.T) {
	withHostPorts(t, "3000")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "8931"}, "3000,8931"},
		{[]string{"add", "8931", "3000"}, "3000,8931"},
		{[]string{"rm", "3000"}, "8931"},
		{[]string{"rm", "3000"}, "8931"},
		{[]string{"set", "4000", "1"}, "1,4000"},
		{[]string{"rm", "1", "4000"}, ""},
	} {
		if err := run(t, append([]string{"remote", "host-ports"}, tc.args...)...); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got := savedWorkspace(t, "dev").HostPorts; got != tc.want {
			t.Errorf("after %v the file says %q, want %q", tc.args, got, tc.want)
		}
	}
	// Another workspace is not touched.
	if got := savedWorkspace(t, "ci").HostPorts; got != "9000" {
		t.Errorf("ci's list changed to %q", got)
	}
}

func TestHostPortsWorkOnTheWorkspaceNamed(t *testing.T) {
	withHostPorts(t, "")
	if err := run(t, "remote", "host-ports", "add", "5000", "--workspace", "ci"); err != nil {
		t.Fatal(err)
	}
	if got := savedWorkspace(t, "ci").HostPorts; got != "5000,9000" {
		t.Errorf("ci = %q, want 5000,9000", got)
	}
	if got := savedWorkspace(t, "dev").HostPorts; got != "" {
		t.Errorf("dev = %q, want it untouched", got)
	}
}

func TestHostPortsRefuseWhatIsNoPort(t *testing.T) {
	withHostPorts(t, "3000")
	for _, arg := range []string{"0", "65536", "http", "-1"} {
		if err := run(t, "remote", "host-ports", "add", "--", arg); err == nil {
			t.Errorf("add %s was accepted", arg)
		}
	}
	if got := savedWorkspace(t, "dev").HostPorts; got != "3000" {
		t.Errorf("a refused add changed the file to %q", got)
	}
}

// It is the same field the setting writes, so the two spellings agree.
func TestHostPortsAreTheSettingsField(t *testing.T) {
	withHostPorts(t, "")
	if err := run(t, "remote", "set", "dev", "--host-ports", "3000,8931"); err != nil {
		t.Fatal(err)
	}
	out, err := runOut(t, "remote", "host-ports", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range []string{"3000", "8931"} {
		if !strings.Contains(out, port+"  listed") {
			t.Errorf("ls does not list %s:\n%s", port, out)
		}
	}
	if err := run(t, "remote", "host-ports", "rm", "3000"); err != nil {
		t.Fatal(err)
	}
	if got := savedWorkspace(t, "dev").HostPorts; got != "8931" {
		t.Errorf("file = %q after set then rm", got)
	}
}

func TestHostPortsListSaysWhenTheEnvironmentOverrides(t *testing.T) {
	withHostPorts(t, "3000")
	t.Setenv(config.EnvHostPorts, "4000")
	out, err := runOut(t, "remote", "host-ports", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, config.EnvHostPorts+" is set") || !strings.Contains(out, "4000") || strings.Contains(out, "3000") {
		t.Errorf("ls under the override printed:\n%s", out)
	}

	// An edit still goes to the file, and says the variable wins.
	out, err = runOut(t, "remote", "host-ports", "add", "5000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "overrides") {
		t.Errorf("add under the override did not say so:\n%s", out)
	}
	if got := savedWorkspace(t, "dev").HostPorts; got != "3000,5000" {
		t.Errorf("file = %q, want 3000,5000", got)
	}
}

func TestHostPortsListWithNoneSaysSo(t *testing.T) {
	withHostPorts(t, "")
	out, err := runOut(t, "remote", "host-ports", "ls")
	if err != nil || !strings.Contains(out, "no host ports listed") {
		t.Errorf("ls = %q, %v", out, err)
	}
}
