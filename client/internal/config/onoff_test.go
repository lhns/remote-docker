package config

import "testing"

func TestParseOnOff(t *testing.T) {
	for in, want := range map[string]bool{"on": true, "ON": true, "true": true, "1": true, "yes": true,
		"off": false, "false": false, "0": false, "no": false} {
		got, err := ParseOnOff(in)
		if err != nil || got != want {
			t.Errorf("ParseOnOff(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseOnOff("maybe"); err == nil {
		t.Error("maybe was accepted")
	}
}

// read-only-exports follows the neighbouring settings: off by default, the
// file turns it on, and the environment wins in both directions (ADR 0058).
func TestReadOnlyExportsPrecedence(t *testing.T) {
	file := Config{}
	applyWorkspace(&file, Workspace{ReadOnlyExports: true})
	if !file.ReadOnlyExports {
		t.Error("the file setting was ignored")
	}
	if (Config{}).ReadOnlyExports {
		t.Error("on by default")
	}

	t.Setenv(EnvReadOnlyExports, "off")
	applyEnv(&file)
	if file.ReadOnlyExports {
		t.Error("the environment did not turn it off")
	}
	t.Setenv(EnvReadOnlyExports, "on")
	var env Config
	applyEnv(&env)
	if !env.ReadOnlyExports || env.SessionSettings()["readonly-exports"] != "on" {
		t.Errorf("the environment did not turn it on: %v", env.SessionSettings()["readonly-exports"])
	}
	t.Setenv(EnvReadOnlyExports, "garbage")
	keep := Config{ReadOnlyExports: true}
	applyEnv(&keep)
	if !keep.ReadOnlyExports {
		t.Error("an unreadable value changed the setting")
	}
}
