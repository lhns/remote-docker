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
// Left alone, that fails `docker run` on any image, including one that needs
// no credentials at all: the helper is consulted before anyone knows whether
// the registry wants authentication.
//
// What to do about it depends on whether anything is actually lost, and the
// config says which. `auths` lists the registries you have logged into even
// when the secrets themselves live in the keychain, so:
//
//   - nothing listed: the helper was holding nothing we know of. Drop it, say
//     so once, and carry on. This is the uninstalled-Docker-Desktop case, and
//     stopping here would block a public image over a stale config line.
//   - registries listed: those logins are unreachable, and pulling from any of
//     them will fail at the registry with a 401 that names nothing. Refuse
//     here instead, where the cause can be named.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
)

// credentialHelperPrefix is how a store name becomes a binary name.
const credentialHelperPrefix = "docker-credential-"

// IgnoreCredentialHelpersEnv proceeds despite unreachable logins.
//
// For the case the rule below cannot judge: a config that lists registries,
// on a run that does not need any of them. The registry names are evidence,
// not proof, and this is the escape hatch for when the evidence is wrong.
const IgnoreCredentialHelpersEnv = "REMOTE_DOCKER_IGNORE_CREDENTIAL_HELPERS"

// checkCredentialHelpers drops helpers that are not installed and reports
// whether the command should continue.
//
// The returned error is for the caller to fail with. Dropping is done either
// way: if the caller proceeds, it proceeds on the file store rather than on a
// helper that cannot run.
func checkCredentialHelpers(cf *configfile.ConfigFile, lookPath func(string) (string, error), warn io.Writer) error {
	if cf == nil {
		return nil
	}

	missing := drop(cf, lookPath)
	if len(missing) == 0 {
		return nil
	}

	// The registries whose logins have just become unreachable. Sorted, so the
	// message is the same twice in a row.
	var lost []string
	for registry := range cf.AuthConfigs {
		lost = append(lost, registry)
	}
	sort.Strings(lost)

	if len(lost) == 0 {
		_, _ = fmt.Fprintf(warn,
			"warning: credential helper %s is not installed; no stored logins were using it.\n"+
				"  fix: remove the \"credsStore\" line from %s\n",
			strings.Join(missing, ", "), configPath())
		return nil
	}

	if os.Getenv(IgnoreCredentialHelpersEnv) != "" {
		_, _ = fmt.Fprintf(warn,
			"warning: credential helper %s is not installed, so logins for %s are unavailable.\n"+
				"  continuing because %s is set\n",
			strings.Join(missing, ", "), strings.Join(lost, ", "), IgnoreCredentialHelpersEnv)
		return nil
	}

	// Named here rather than left to the registry, which answers 401 and says
	// nothing about why the credentials were missing.
	//
	// ONE fix, then the way to carry on without one. "Install the helper" is
	// deliberately not offered: docker-credential-desktop ships with Docker
	// Desktop, which is the software this project exists because you cannot
	// install, and a different helper would not reach the keychain the old one
	// wrote to. Those secrets are unreachable whatever this message says, so
	// the only thing that ends the problem is editing the file.
	return fmt.Errorf(
		"credential helper %s is not installed, so the stored logins for %s cannot be read.\n"+
			"  fix: remove the \"credsStore\" line from %s, then `docker login` for each of them\n"+
			"  or:  set %s=1 to carry on without those logins",
		strings.Join(missing, ", "), strings.Join(lost, ", "), configPath(),
		IgnoreCredentialHelpersEnv)
}

// configPath is the file to edit, spelled the way this machine spells it.
//
// Named rather than described. "~/.docker/config.json" is not where it is on
// Windows, and an instruction whose first step is working out which file it
// means is most of an instruction.
func configPath() string {
	path, err := dockerconfig.Path(dockerconfig.ConfigFileName)
	if err != nil {
		return filepath.Join(dockerconfig.Dir(), dockerconfig.ConfigFileName)
	}
	return path
}

// drop removes every helper that is not installed and returns their binary
// names.
func drop(cf *configfile.ConfigFile, lookPath func(string) (string, error)) []string {
	var missing []string

	if store := cf.CredentialsStore; store != "" && !helperInstalled(store, lookPath) {
		cf.CredentialsStore = ""
		missing = append(missing, credentialHelperPrefix+store)
	}
	for registry, store := range cf.CredentialHelpers {
		if helperInstalled(store, lookPath) {
			continue
		}
		delete(cf.CredentialHelpers, registry)
		missing = append(missing, credentialHelperPrefix+store)
	}

	sort.Strings(missing)
	return missing
}

func helperInstalled(store string, lookPath func(string) (string, error)) bool {
	_, err := lookPath(credentialHelperPrefix + store)
	return err == nil
}

// lookPath is exec.LookPath, replaced in tests. Naming it here keeps the
// caller from having to pass it at every call site.
var lookPath = exec.LookPath
