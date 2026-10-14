package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/lhns/remote-docker/core/workspace"
)

// SessionSettings is what a background session reads once, at start: the
// settings a later command can set and have silently ignored until the session
// restarts. The session reports it and a command compares against it, so both
// ends spell a value the same way (ADR 0017, amended 2026-10-14).
//
// Names are the user-facing spelling. An empty watch or prefetch is "off",
// which is what it means, and the share owner is always uid:gid, so 1000,
// 1000:1000 and 1000.1000 compare equal.
func (c Config) SessionSettings() map[string]string {
	paths := make([]string, 0, len(c.ConsistencyPaths))
	for dir, mode := range c.ConsistencyPaths {
		paths = append(paths, dir+"="+mode)
	}
	sort.Strings(paths)

	return map[string]string{
		"consistency":       c.Consistency,
		"consistency-paths": strings.Join(paths, ";"),
		"watch":             offIfEmpty(c.Watch),
		"watch-budget":      fmt.Sprint(c.WatchBudget),
		"watch-exclude":     strings.Join(c.WatchExclude, ","),
		"cache-files":       fmt.Sprint(c.CacheFiles),
		"cache-bytes":       fmt.Sprint(c.CacheBytes),
		"prefetch":          offIfEmpty(c.Prefetch),
		"share-owner":       c.shareOwnerSetting(),
		"idle-timeout":      c.IdleTimeout.String(),
		"daemon-idle":       c.DaemonIdle.String(),
		"daemon-standby":    c.DaemonStandby.String(),
	}
}

// shareOwnerSetting is the owner in its one spelling. Read from the string
// when there is one, because Resolve parses it into ShareUID and ShareGID and a
// Config built by hand may not have.
func (c Config) shareOwnerSetting() string {
	if c.ShareOwner == "" {
		return workspace.Owner{UID: c.ShareUID, GID: c.ShareGID, Set: true}.String()
	}
	if o, err := workspace.ParseOwner(c.ShareOwner); err == nil {
		return o.String()
	}
	return c.ShareOwner
}

func offIfEmpty(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

// notSessionSettings is every Config field SessionSettings leaves out, and
// why. A new field must be in one place or the other (settings_test.go).
var notSessionSettings = map[string]string{
	"Name":             "which workspace, not how it behaves; a session serves one",
	"Host":             "connection identity: another host is another workspace",
	"Port":             "connection identity",
	"CAFile":           "connection identity",
	"Insecure":         "connection identity",
	"Machine":          "connection identity, located on every connect",
	"User":             "connection identity",
	"Endpoint":         "where the session is found, so it cannot differ from the one asked",
	"ShareUID":         "ShareOwner, parsed",
	"ShareGID":         "ShareOwner, parsed",
	"shareOwnerSource": "names where ShareOwner came from, for an error",
}

// SettingChange is one setting whose value differs between two sessions.
type SettingChange struct {
	Name, Running, Ours string
}

// settingEnv is the environment variable that sets each session setting. A
// setting with none is read from the config file only, and is never compared.
var settingEnv = map[string]string{
	"consistency":    EnvConsistency,
	"watch":          EnvWatch,
	"watch-budget":   EnvWatchBudget,
	"watch-exclude":  EnvWatchExclude,
	"cache-files":    EnvCacheFiles,
	"cache-bytes":    EnvCacheBytes,
	"prefetch":       EnvPrefetch,
	"share-owner":    EnvShareOwner,
	"idle-timeout":   EnvIdleTimeout,
	"daemon-idle":    EnvDaemonIdle,
	"daemon-standby": EnvDaemonStandby,
}

// SetInEnvironment reports whether this process's environment names the
// setting. An absent variable is no opinion, not a request for the default.
func SetInEnvironment(name string) bool {
	env, ok := settingEnv[name]
	return ok && os.Getenv(env) != ""
}

// DiffSettings lists the settings in ours that differ from running, by name,
// and for which named(name) is true. A name running does not report is not
// compared: an older session that predates it cannot say, and is left alone.
func DiffSettings(running, ours map[string]string, named func(string) bool) []SettingChange {
	var out []SettingChange
	for name, want := range ours {
		if !named(name) {
			continue
		}
		if have, ok := running[name]; ok && have != want {
			out = append(out, SettingChange{name, have, want})
		}
	}
	slices.SortFunc(out, func(a, b SettingChange) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// NonDefaultSettings lists the session settings that are not the defaults, as
// name=value.
func NonDefaultSettings(settings map[string]string) []string {
	defaults := Config{}.SessionSettings()
	var out []string
	for name, v := range settings {
		if d, ok := defaults[name]; ok && d != v {
			out = append(out, name+"="+v)
		}
	}
	sort.Strings(out)
	return out
}
