package main

// Credential helpers named in config.json but not installed, such as Docker
// Desktop's "desktop". Docker consults the helper before every pull, so a
// missing one fails even public images. `auths` lists logged-in registries
// even when the secrets live in the helper:
//   - none listed: drop the helper, warn, carry on.
//   - some listed: refuse here, since a pull would fail with a bare 401.

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
)

// credentialHelperPrefix is how a store name becomes a binary name.
const credentialHelperPrefix = "docker-credential-"

// IgnoreCredentialHelpersEnv proceeds despite unreachable logins, for a run
// that needs none of the listed registries.
const IgnoreCredentialHelpersEnv = "REMOTE_DOCKER_IGNORE_CREDENTIAL_HELPERS"

// checkCredentialHelpers drops helpers that are not installed, and returns an
// error if logins were lost. The drop happens either way.
func checkCredentialHelpers(cf *configfile.ConfigFile, lookPath func(string) (string, error), warn io.Writer) error {
	if cf == nil {
		return nil
	}

	missing := drop(cf, lookPath)
	if len(missing) == 0 {
		return nil
	}

	lost := slices.Sorted(maps.Keys(cf.AuthConfigs))

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

	// "Install the helper" is not offered: it ships with Docker Desktop, and
	// another helper cannot read the old one's keychain.
	return fmt.Errorf(
		"credential helper %s is not installed, so the stored logins for %s cannot be read.\n"+
			"  fix: remove the \"credsStore\" line from %s, then `docker login` for each of them\n"+
			"  or:  set %s=1 to carry on without those logins",
		strings.Join(missing, ", "), strings.Join(lost, ", "), configPath(),
		IgnoreCredentialHelpersEnv)
}

// configPath is config.json as this machine spells it.
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

	slices.Sort(missing)
	return missing
}

func helperInstalled(store string, lookPath func(string) (string, error)) bool {
	_, err := lookPath(credentialHelperPrefix + store)
	return err == nil
}
