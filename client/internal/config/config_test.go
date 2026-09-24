package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "remote-docker.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Precedence is command line, then environment, then file, then default.
func TestResolvePrecedence(t *testing.T) {
	path := writeConfig(t, `{"host":"from-file","port":2000,"user":"file-user"}`)

	t.Run("file only", func(t *testing.T) {
		cfg, err := Resolve(Overrides{}, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "from-file" || cfg.Port != 2000 || cfg.User != "file-user" {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("environment beats file", func(t *testing.T) {
		t.Setenv(EnvHost, "from-env")
		t.Setenv(EnvPort, "3000")
		cfg, err := Resolve(Overrides{}, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "from-env" || cfg.Port != 3000 {
			t.Errorf("got %+v", cfg)
		}
		if cfg.User != "file-user" {
			t.Errorf("User = %q; an unset environment variable must not clear the file", cfg.User)
		}
	})

	t.Run("flags beat everything", func(t *testing.T) {
		t.Setenv(EnvHost, "from-env")
		cfg, err := Resolve(Overrides{Host: "from-flag", Port: 4000}, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Host != "from-flag" || cfg.Port != 4000 {
			t.Errorf("got %+v", cfg)
		}
	})
}

// A port in an ssh:// host is the port, through Resolve as well as Transport:
// the SSH default must not be filled in and then read as a `port` setting
// that contradicts the URL.
func TestResolveKeepsThePortOfAnSSHURL(t *testing.T) {
	path := writeConfig(t, `{"host":"ssh://dev.example:2299"}`)
	cfg, err := Resolve(Overrides{}, path)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := cfg.Transport()
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	if transport.Port != 2299 {
		t.Errorf("port = %d, want 2299", transport.Port)
	}
}

func TestResolveDefaults(t *testing.T) {
	cfg, err := Resolve(Overrides{}, filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if cfg.Port != DefaultSSHPort {
		t.Errorf("Port = %d, want %d", cfg.Port, DefaultSSHPort)
	}
	if cfg.User == "" {
		t.Error("User is empty; it should fall back to the local username")
	}
	if cfg.Host != "" {
		t.Errorf("Host = %q; there is no sensible default workspace", cfg.Host)
	}
}

// enroll has to work before anything is configured, since that is how a key gets
// issued in the first place, so an absent file cannot be fatal.
func TestResolveToleratesMissingFile(t *testing.T) {
	if _, err := Resolve(Overrides{}, filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Errorf("Resolve with no config file: %v", err)
	}
}

func TestResolveRejectsMalformedFile(t *testing.T) {
	path := writeConfig(t, `{"host": `)
	if _, err := Resolve(Overrides{}, path); err == nil {
		t.Error("a malformed config file was accepted")
	}
}

// A bad port in the environment must not break commands that never connect.
func TestResolveIgnoresMalformedEnvPort(t *testing.T) {
	t.Setenv(EnvPort, "not-a-number")
	cfg, err := Resolve(Overrides{}, filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Port != DefaultSSHPort {
		t.Errorf("Port = %d, want the default %d", cfg.Port, DefaultSSHPort)
	}
}

func TestResolveRejectsOutOfRangePort(t *testing.T) {
	for _, port := range []int{-1, 70000} {
		if _, err := Resolve(Overrides{Port: port}, filepath.Join(t.TempDir(), "absent.json")); err == nil {
			t.Errorf("port %d was accepted", port)
		}
	}
}

func TestRequireHost(t *testing.T) {
	if err := (Config{Host: "workspace"}).RequireHost(); err != nil {
		t.Errorf("a configured host was rejected: %v", err)
	}

	if err := (Config{User: "alice"}).RequireHost(); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("a missing host answered %v, want ErrNoWorkspace", err)
	}
}

func TestSanitizeUser(t *testing.T) {
	tests := map[string]string{
		"alice":         "alice",
		"Alice":         "alice",
		"ALICE":         "alice",
		"alice.smith":   "alice-smith",
		"DOMAIN\\alice": "domain-alice",
		"alice@example": "alice-example",
		"123alice":      "alice",
		"_alice":        "_alice",
		"":              "user",
		"123":           "user",
		"---":           "user",
	}
	for in, want := range tests {
		if got := sanitizeUser(in); got != want {
			t.Errorf("sanitizeUser(%q) = %q, want %q", in, got, want)
		}
	}

	if got := sanitizeUser(strings.Repeat("a", 50)); len(got) != 30 {
		t.Errorf("a long username produced %d characters, want 30", len(got))
	}
}

func TestResolveNamedWorkspaces(t *testing.T) {
	path := writeConfig(t, `{
		"user": "shared-user",
		"workspaces": {
			"dev": {"host": "dev.example"},
			"ci":  {"host": "ci.example", "port": 2223, "user": "ci-user"}
		},
		"default": "dev"
	}`)

	t.Run("default is used when none is named", func(t *testing.T) {
		cfg, err := Resolve(Overrides{}, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Name != "dev" || cfg.Host != "dev.example" {
			t.Errorf("got %+v", cfg)
		}
		// Top-level fields are shared, so settings common to every workspace
		// need not be repeated.
		if cfg.User != "shared-user" {
			t.Errorf("User = %q, want the shared top-level value", cfg.User)
		}
	})

	t.Run("a named workspace overrides the shared fields", func(t *testing.T) {
		cfg, err := Resolve(Overrides{Workspace: "ci"}, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Name != "ci" || cfg.Host != "ci.example" || cfg.Port != 2223 || cfg.User != "ci-user" {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("the environment can select one", func(t *testing.T) {
		t.Setenv(EnvWorkspace, "ci")
		cfg, err := Resolve(Overrides{}, path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Name != "ci" {
			t.Errorf("Name = %q, want ci", cfg.Name)
		}
	})

	// The remedy names one of our commands, so the caller adds it.
	t.Run("an unknown name is ErrUnknownWorkspace", func(t *testing.T) {
		_, err := Resolve(Overrides{Workspace: "nope"}, path)
		if !errors.Is(err, ErrUnknownWorkspace) || err.Error() != `no workspace named "nope"` {
			t.Errorf("an unknown workspace answered %v", err)
		}
	})
}

// With one workspace configured, not naming it is unambiguous rather than
// lazy, so it should not need a "default" entry.
func TestResolveSingleNamedWorkspaceNeedsNoDefault(t *testing.T) {
	path := writeConfig(t, `{"workspaces": {"only": {"host": "only.example"}}}`)
	cfg, err := Resolve(Overrides{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "only" || cfg.Host != "only.example" {
		t.Errorf("got %+v", cfg)
	}
}

// With several and no default, guessing would be worse than asking.
func TestResolveAmbiguousWorkspacesAreRefused(t *testing.T) {
	path := writeConfig(t, `{"workspaces": {"a": {"host": "a"}, "b": {"host": "b"}}}`)
	_, err := Resolve(Overrides{}, path)
	if err == nil {
		t.Fatal("an ambiguous config was accepted")
	}
	if !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("error %q does not say how to resolve it", err)
	}
}

// The flat single-workspace form must keep working untouched.
func TestResolveFlatFormStillWorks(t *testing.T) {
	path := writeConfig(t, `{"host": "solo.example", "user": "alice"}`)
	cfg, err := Resolve(Overrides{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "" || cfg.Host != "solo.example" || cfg.User != "alice" {
		t.Errorf("got %+v", cfg)
	}
}

// Each workspace needs its own endpoint, or two sessions would fight over one
// pipe and the second would fail to start.
func TestEndpointForIsPerWorkspace(t *testing.T) {
	base := "base"

	solo := Config{}.EndpointFor(base)
	dev := Config{Name: "dev"}.EndpointFor(base)
	ci := Config{Name: "ci"}.EndpointFor(base)

	if solo != base {
		t.Errorf("unnamed workspace endpoint = %q, want the base %q", solo, base)
	}
	if dev == ci || dev == base || ci == base {
		t.Errorf("endpoints collide: solo=%q dev=%q ci=%q", solo, dev, ci)
	}

	// An explicit endpoint is the user's decision and is not derived over.
	explicit := Config{Name: "dev", Endpoint: "chosen"}.EndpointFor(base)
	if explicit != "chosen" {
		t.Errorf("explicit endpoint = %q, want it respected", explicit)
	}
}

// An empty base has no name to build on, and building one anyway was a real
// bug: DefaultEndpoint was "" on Unix, resolved inside Listen, which was fine
// for Listen and wrong for everyone else, so a NAMED workspace derived the
// bare separator plus its name. "-dev" is a socket in whatever directory the
// process happened to be in, and the docker context written from it said
// unix://-dev. It could not be reproduced on Windows, where the pipe name is a
// real constant, and CI never saw it because the suite sets an endpoint
// explicitly.
func TestEndpointForDoesNotBuildARelativePathFromNothing(t *testing.T) {
	got := Config{Name: "dev"}.EndpointFor("")
	if got != "" {
		t.Errorf("EndpointFor(\"\") = %q for a named workspace, want the default; "+
			"anything else names something relative to the working directory", got)
	}
}

func TestContextName(t *testing.T) {
	if got := (Config{}).ContextName(); got != "remote-docker" {
		t.Errorf("unnamed context = %q", got)
	}
	if got := (Config{Name: "dev"}).ContextName(); got != "dev" {
		t.Errorf("named context = %q, want dev", got)
	}
}
