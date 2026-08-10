package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/cli/cli/config/configfile"
)

// none is a PATH with no credential helpers on it, which is what a machine
// that has never had Docker installed looks like.
func none(string) (string, error) { return "", errors.New("not found") }

// all is a PATH with every helper on it.
func all(name string) (string, error) { return name, nil }

// A helper that is named but not installed must not be consulted. Docker
// Desktop leaves "credsStore": "desktop" behind, the binary goes with it, and
// every pull then fails on credentials it never needed.
func TestAMissingStoreIsDropped(t *testing.T) {
	cf := &configfile.ConfigFile{CredentialsStore: "desktop"}
	var warn bytes.Buffer

	dropMissingCredentialHelpers(cf, none, &warn)

	if cf.CredentialsStore != "" {
		t.Errorf("CredentialsStore = %q, want it cleared", cf.CredentialsStore)
	}
	for _, want := range []string{"docker-credential-desktop", "fix:"} {
		if !strings.Contains(warn.String(), want) {
			t.Errorf("the warning does not mention %q:\n%s", want, warn.String())
		}
	}
}

// An installed one is left exactly alone: it holds the user's real logins, and
// this must never be the reason a working setup stops working.
func TestAnInstalledStoreIsKept(t *testing.T) {
	cf := &configfile.ConfigFile{
		CredentialsStore: "wincred",
		CredentialHelpers: map[string]string{
			"registry.example.org": "pass",
		},
	}
	var warn bytes.Buffer

	dropMissingCredentialHelpers(cf, all, &warn)

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

// Per-registry helpers are dropped one at a time. A machine can have one
// helper and not another, and dropping the working one would lose a login
// that was about to succeed.
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
	var warn bytes.Buffer

	dropMissingCredentialHelpers(cf, present, &warn)

	if _, ok := cf.CredentialHelpers["gone.example.org"]; ok {
		t.Error("a missing helper was kept")
	}
	if cf.CredentialHelpers["present.example.org"] != "wincred" {
		t.Error("an installed helper was dropped with it")
	}
	if !strings.Contains(warn.String(), "gone.example.org") {
		t.Errorf("the warning does not name the registry:\n%s", warn.String())
	}
}

// A config with no helpers at all is the ordinary case and must be silent.
func TestNothingConfiguredSaysNothing(t *testing.T) {
	var warn bytes.Buffer
	dropMissingCredentialHelpers(&configfile.ConfigFile{}, none, &warn)
	if warn.Len() != 0 {
		t.Errorf("an empty configuration warned:\n%s", warn.String())
	}

	// And a nil config file, which is what a CLI that failed to initialise
	// hands back.
	dropMissingCredentialHelpers(nil, none, io.Discard)
}
