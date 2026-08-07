package config

import (
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

// enroll has to work before anything is configured -- that is how a key gets
// issued in the first place -- so an absent file cannot be fatal.
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

	err := (Config{User: "alice"}).RequireHost()
	if err == nil {
		t.Fatal("a missing host was accepted")
	}
	// The message has to say how to fix it, not just that it is wrong.
	for _, want := range []string{EnvHost, "--host", "alice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
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
