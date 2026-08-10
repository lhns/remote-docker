package main

// Credential helpers that are named but not installed.
//
// ~/.docker/config.json can name a credential helper, and Docker Desktop puts
// its own there ("credsStore": "desktop"). The helper is a separate binary,
// and on a machine that does not have Docker installed, which is the premise
// of this project, it is not there:
//
//	docker: error getting credentials - err: exec:
//	"docker-credential-desktop": executable file not found in %PATH%
//
// That fails the pull, so `docker run` on any image fails, including one that
// needs no credentials at all. The helper is consulted before anyone knows
// whether the registry wants authentication.

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/docker/cli/cli/config/configfile"
)

// credentialHelperPrefix is how a store name becomes a binary name.
const credentialHelperPrefix = "docker-credential-"

// dropMissingCredentialHelpers removes credential helpers that are not
// installed, in memory, and says which and why.
//
// Falling back to the config file's own `auths` is what the docker CLI does
// when no helper is configured, so this is a downgrade to the ordinary path
// rather than an invention. It cannot recover secrets the missing helper was
// holding, and nothing can: they are in a keychain this binary has no way to
// read. What it does recover is every pull that needed no credentials.
func dropMissingCredentialHelpers(cf *configfile.ConfigFile, lookPath func(string) (string, error), warn io.Writer) {
	if cf == nil {
		return
	}

	if store := cf.CredentialsStore; store != "" && !helperInstalled(store, lookPath) {
		cf.CredentialsStore = ""
		_, _ = fmt.Fprintf(warn,
			"warning: credential helper %q is not installed, so stored logins are unavailable.\n"+
				"  fix: `docker login` writes to the config file instead, or remove \"credsStore\" from it\n",
			credentialHelperPrefix+store)
	}

	for registry, store := range cf.CredentialHelpers {
		if helperInstalled(store, lookPath) {
			continue
		}
		delete(cf.CredentialHelpers, registry)
		_, _ = fmt.Fprintf(warn,
			"warning: credential helper %q for %s is not installed, so its stored login is unavailable.\n"+
				"  fix: `docker login %s` writes to the config file instead\n",
			credentialHelperPrefix+store, registry, registry)
	}
}

func helperInstalled(store string, lookPath func(string) (string, error)) bool {
	_, err := lookPath(credentialHelperPrefix + store)
	return err == nil
}

// lookPath is exec.LookPath, replaced in tests. Naming it here keeps the
// caller from having to pass it at every call site.
var lookPath = exec.LookPath
