package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
)

// none is a PATH with no credential helpers on it, which is what a machine
// that has never had Docker installed looks like.
func none(string) (string, error) { return "", errors.New("not found") }

// all is a PATH with every helper on it.
func all(name string) (string, error) { return name, nil }

// withLogins is a config that names registries, which is what docker writes
// when you have logged in: the registry list stays in `auths` even though the
// secret itself went to the keychain.
func withLogins(registries ...string) map[string]types.AuthConfig {
	auths := map[string]types.AuthConfig{}
	for _, r := range registries {
		auths[r] = types.AuthConfig{}
	}
	return auths
}

// Nothing stored, so nothing is lost: warn and carry on. This is the machine
// that had Docker Desktop once, and it must still be able to run a public
// image, which needs no credentials at all.
func TestAMissingHelperWithNoLoginsOnlyWarns(t *testing.T) {
	cf := &configfile.ConfigFile{CredentialsStore: "desktop"}
	var warn bytes.Buffer

	if err := checkCredentialHelpers(cf, none, &warn); err != nil {
		t.Fatalf("a helper holding nothing stopped the command: %v", err)
	}
	if cf.CredentialsStore != "" {
		t.Errorf("CredentialsStore = %q, want it cleared", cf.CredentialsStore)
	}
	if !strings.Contains(warn.String(), "docker-credential-desktop") {
		t.Errorf("the warning does not name the helper:\n%s", warn.String())
	}
}

// Logins are stored and now unreachable, so stop where the cause can be named
// rather than at a registry answering 401.
func TestAMissingHelperWithLoginsStops(t *testing.T) {
	cf := &configfile.ConfigFile{
		CredentialsStore: "desktop",
		AuthConfigs:      withLogins("registry.example.org", "ghcr.io"),
	}

	err := checkCredentialHelpers(cf, none, io.Discard)
	if err == nil {
		t.Fatal("unreachable logins were passed over in silence")
	}
	// The registries, so the reader knows what they have lost.
	for _, want := range []string{"registry.example.org", "ghcr.io", "docker-credential-desktop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q:\n%v", want, err)
		}
	}
	// The FILE, because "remove the credsStore line" is not an instruction
	// until you know which file it is in. It is not ~/.docker on Windows.
	if !strings.Contains(err.Error(), configPath()) {
		t.Errorf("the error does not name the file to edit:\n%v", err)
	}
	if !strings.Contains(err.Error(), IgnoreCredentialHelpersEnv) {
		t.Errorf("the error does not say how to carry on anyway:\n%v", err)
	}
}

// The escape hatch, for a config that lists registries on a run that needs
// none of them.
func TestTheEscapeHatchCarriesOn(t *testing.T) {
	cf := &configfile.ConfigFile{
		CredentialsStore: "desktop",
		AuthConfigs:      withLogins("registry.example.org"),
	}
	t.Setenv(IgnoreCredentialHelpersEnv, "1")

	var warn bytes.Buffer
	if err := checkCredentialHelpers(cf, none, &warn); err != nil {
		t.Fatalf("%s did not carry on: %v", IgnoreCredentialHelpersEnv, err)
	}
	if !strings.Contains(warn.String(), "registry.example.org") {
		t.Errorf("carrying on said nothing about what is unavailable:\n%s", warn.String())
	}
}

// An installed helper is left exactly alone, logins or not. This must never be
// the reason a working setup stops working.
func TestAnInstalledHelperIsUntouched(t *testing.T) {
	cf := &configfile.ConfigFile{
		CredentialsStore:  "wincred",
		CredentialHelpers: map[string]string{"registry.example.org": "pass"},
		AuthConfigs:       withLogins("registry.example.org"),
	}
	var warn bytes.Buffer

	if err := checkCredentialHelpers(cf, all, &warn); err != nil {
		t.Fatalf("a working configuration was refused: %v", err)
	}
	if cf.CredentialsStore != "wincred" {
		t.Errorf("CredentialsStore = %q, want it untouched", cf.CredentialsStore)
	}
	if cf.CredentialHelpers["registry.example.org"] != "pass" {
		t.Error("an installed per-registry helper was dropped")
	}
	if warn.Len() != 0 {
		t.Errorf("a working configuration warned anyway:\n%s", warn.String())
	}
}

// Per-registry helpers are dropped one at a time: a machine can have one and
// not another, and taking the working one would lose a login that was about to
// succeed.
func TestOnlyTheMissingPerRegistryHelpersAreDropped(t *testing.T) {
	cf := &configfile.ConfigFile{
		CredentialHelpers: map[string]string{
			"gone.example.org":    "desktop",
			"present.example.org": "wincred",
		},
	}
	present := func(name string) (string, error) {
		if name == "docker-credential-wincred" {
			return name, nil
		}
		return "", errors.New("not found")
	}

	if err := checkCredentialHelpers(cf, present, io.Discard); err != nil {
		t.Fatalf("no logins were stored, so this should not have stopped: %v", err)
	}
	if _, ok := cf.CredentialHelpers["gone.example.org"]; ok {
		t.Error("a missing helper was kept")
	}
	if cf.CredentialHelpers["present.example.org"] != "wincred" {
		t.Error("an installed helper was dropped with it")
	}
}

// A config with no helpers at all is the ordinary case and must be silent.
func TestNothingConfiguredSaysNothing(t *testing.T) {
	var warn bytes.Buffer
	if err := checkCredentialHelpers(&configfile.ConfigFile{}, none, &warn); err != nil {
		t.Fatalf("an empty configuration was refused: %v", err)
	}
	if warn.Len() != 0 {
		t.Errorf("an empty configuration warned:\n%s", warn.String())
	}

	// And a nil config file, which is what a CLI that failed to initialise
	// hands back.
	if err := checkCredentialHelpers(nil, none, io.Discard); err != nil {
		t.Errorf("a nil config was refused: %v", err)
	}
}

// The file has to be named, and named as this machine spells it.
func TestTheConfigPathIsAbsolute(t *testing.T) {
	path := configPath()
	if !strings.HasSuffix(path, "config.json") {
		t.Errorf("configPath() = %q, want it to end at the config file", path)
	}
	if strings.HasPrefix(path, "~") {
		t.Errorf("configPath() = %q, which nobody can open", path)
	}
}
