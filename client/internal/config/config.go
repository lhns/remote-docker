// Package config resolves how to reach a workspace, and where this client
// keeps its state.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultSSHPort is the workspace's sshd port.
const DefaultSSHPort = 2222

// Config is everything needed to open a session.
type Config struct {
	// Name identifies the workspace among several. Empty means the single
	// unnamed workspace of a flat config file.
	Name string

	// Host is the workspace's address. There is no default; without it there
	// is nothing to connect to.
	Host string

	// Port is the workspace's SSH port.
	Port int

	// User is the workspace account, which is also the name of the .pub file
	// enrolled for this machine.
	User string

	// Endpoint is where the local Docker API is served. Empty means the
	// platform default.
	Endpoint string

	// Watch is how much of this machine's filesystem activity to replay into
	// the workspace, so watchers in containers notice edits made here:
	// "off" (the default), "partial" or "coarse". See ADR 0016.
	//
	// Held as the raw string rather than a parsed mode so this package stays
	// the lowest layer, depending on nothing above it. The command parses and
	// reports a bad value.
	Watch string

	// WatchBudget caps how many directories are watched at once. Zero means
	// the per-platform default, which differs because the limit that binds
	// differs: inotify watches on Linux, buffers on Windows, file descriptors
	// on macOS.
	WatchBudget int

	// WatchExclude replaces the default list of directory names never
	// watched. Empty means the default.
	WatchExclude []string

	// IdleTimeout is how long the workspace connection may sit unused before
	// being released (ADR 0015). Zero means the default; negative never
	// releases. Configurable chiefly so the integration suite can exercise
	// idle release without sleeping past a fixed minute, but a slow link is a
	// fair reason to raise it too.
	IdleTimeout time.Duration

	// DaemonIdle is how long a background session may sit with nothing to do
	// before it exits. Zero uses DefaultDaemonIdle; negative never exits.
	//
	// Longer than IdleTimeout by a lot, and doing something different: that
	// one drops a connection which reopens on the next request, this one ends
	// a process that cannot come back on its own. It never fires while
	// anything depends on the session.
	DaemonIdle time.Duration
}

// DefaultDaemonIdle is how long a background session outlives its last use.
//
// Half an hour: long enough that stepping away from a task and coming back
// finds it still up, short enough that a workspace opened once last week is
// not still holding a socket and a watch.
const DefaultDaemonIdle = 30 * time.Minute

// File is the on-disk form, ~/.remote-docker.json.
//
// Two shapes are accepted. A flat one describes a single workspace:
//
//	{"host": "dev.example", "user": "alice"}
//
// and a keyed one describes several:
//
//	{"workspaces": {"dev": {...}, "ci": {...}}, "default": "dev"}
//
// The flat form is not deprecated. Most people have one workspace, and making
// them nest it under a name to say so would be a poor trade.
type File struct {
	// The flat form IS a workspace, so it is one. Embedded rather than
	// repeated: these seven fields were declared identically in both types,
	// and two appliers walked them separately, so adding a setting meant
	// remembering four places. encoding/json inlines an embedded struct's
	// fields, tags and all, so the file format is unchanged.
	Workspace

	Workspaces map[string]Workspace `json:"workspaces,omitempty"`
	Default    string               `json:"default,omitempty"`
}

// Workspace is one entry in the keyed form.
//
// The two durations are held as strings because that is what the file says --
// "90s", "45m", and encoding/json has no idea what a time.Duration is. They
// are parsed in applyWorkspace, where a malformed one is ignored rather than
// fatal, exactly as the environment's are.
type Workspace struct {
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	User         string   `json:"user,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Watch        string   `json:"watch,omitempty"`
	WatchBudget  int      `json:"watchBudget,omitempty"`
	WatchExclude []string `json:"watchExclude,omitempty"`
	IdleTimeout  string   `json:"idleTimeout,omitempty"`
	DaemonIdle   string   `json:"daemonIdle,omitempty"`
}

// Names lists the configured workspaces in a stable order.
func (f File) Names() []string {
	names := make([]string, 0, len(f.Workspaces))
	for name := range f.Workspaces {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// selected picks which workspace a request refers to.
//
// An explicit name wins; then the file's stated default; then, only when there
// is exactly one, that one, because with a single workspace configured, not
// naming it is unambiguous rather than lazy.
func (f File) selected(want string) (string, Workspace, error) {
	if len(f.Workspaces) == 0 {
		if want != "" {
			return "", Workspace{}, fmt.Errorf("config: no workspaces are configured, so %q cannot be selected", want)
		}
		return "", Workspace{}, nil
	}

	switch {
	case want != "":
	case f.Default != "":
		want = f.Default
	case len(f.Workspaces) == 1:
		want = f.Names()[0]
	default:
		return "", Workspace{}, fmt.Errorf(
			"config: several workspaces are configured (%s) and none is the default; "+
				"pass --workspace, set %s, or add a \"default\" to %s",
			strings.Join(f.Names(), ", "), EnvWorkspace, DefaultPath())
	}

	ws, ok := f.Workspaces[want]
	if !ok {
		return "", Workspace{}, fmt.Errorf(
			"config: no workspace named %q; configured: %s",
			want, strings.Join(f.Names(), ", "))
	}
	return want, ws, nil
}

// Overrides are values supplied on the command line. Zero values mean
// "not specified" and fall through to the next source.
type Overrides struct {
	Workspace string
	Host      string
	Port      int
	User      string
	Endpoint  string
	Watch     string
}

// Environment variable names.
const (
	EnvHost      = "REMOTE_DOCKER_HOST"
	EnvPort      = "REMOTE_DOCKER_PORT"
	EnvUser      = "REMOTE_DOCKER_USER"
	EnvEndpoint  = "REMOTE_DOCKER_ENDPOINT"
	EnvWorkspace = "REMOTE_DOCKER_WORKSPACE"

	EnvWatch        = "REMOTE_DOCKER_WATCH"
	EnvWatchBudget  = "REMOTE_DOCKER_WATCH_BUDGET"
	EnvWatchExclude = "REMOTE_DOCKER_WATCH_EXCLUDE"
	EnvIdleTimeout  = "REMOTE_DOCKER_IDLE_TIMEOUT"
	EnvDaemonIdle   = "REMOTE_DOCKER_DAEMON_IDLE"
)

// Resolve combines the sources in order of decreasing precedence: command
// line, environment, config file, defaults.
//
// The file is optional and a missing one is not an error: `enroll` has to work
// before anything is configured, since that is how a key gets issued.
func Resolve(o Overrides, path string) (Config, error) {
	cfg := Config{Port: DefaultSSHPort, User: defaultUser()}

	file, err := Load(path)
	if err != nil {
		return Config{}, err
	}

	want := o.Workspace
	if want == "" {
		want = os.Getenv(EnvWorkspace)
	}
	name, ws, err := file.selected(want)
	if err != nil {
		return Config{}, err
	}
	cfg.Name = name

	applyWorkspace(&cfg, file.Workspace)
	applyWorkspace(&cfg, ws)
	applyEnv(&cfg)
	applyOverrides(&cfg, o)

	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("config: port %d is not valid", cfg.Port)
	}
	return cfg, nil
}

// readRetries is how many times a read is attempted before its error is
// believed.
//
// Windows only, in practice, and for a specific transient: while Save renames
// the new file over the old one, an opener can be refused with a sharing
// violation. It is brief and it clears; reporting it would mean
// `remote-docker status` failing because a session happened to be writing its
// config at that instant.
//
// Deliberately small, and deliberately not applied to a MISSING file, which is
// answered immediately as the empty config it means. Retrying a real
// permission problem three times over 30ms costs nothing and tells the user
// the same thing in the end.
const (
	readRetries = 3
	readBackoff = 10 * time.Millisecond
)

// Load reads the config file. A missing file yields a zero File and no error.
func Load(path string) (File, error) {
	if path == "" {
		path = DefaultPath()
	}

	var (
		data []byte
		err  error
	)
	for attempt := range readRetries {
		data, err = os.ReadFile(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			break
		}
		if attempt < readRetries-1 {
			time.Sleep(readBackoff)
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return File{}, nil
		}
		return File{}, fmt.Errorf("config: reading %s: %w", path, err)
	}

	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	return file, nil
}

// applyWorkspace overlays a workspace's settings, leaving anything it does not
// set alone.
//
// Called twice: once for the file's flat fields, then once for the named entry
// on top, so shared settings can sit at the top level and be specialised per
// entry. It used to be two functions with the same seven clauses.
func applyWorkspace(cfg *Config, ws Workspace) {
	if ws.Host != "" {
		cfg.Host = ws.Host
	}
	if ws.Port != 0 {
		cfg.Port = ws.Port
	}
	if ws.User != "" {
		cfg.User = ws.User
	}
	if ws.Endpoint != "" {
		cfg.Endpoint = ws.Endpoint
	}
	if ws.Watch != "" {
		cfg.Watch = ws.Watch
	}
	if ws.WatchBudget != 0 {
		cfg.WatchBudget = ws.WatchBudget
	}
	if len(ws.WatchExclude) > 0 {
		cfg.WatchExclude = ws.WatchExclude
	}
	if d, ok := duration(ws.IdleTimeout); ok {
		cfg.IdleTimeout = d
	}
	if d, ok := duration(ws.DaemonIdle); ok {
		cfg.DaemonIdle = d
	}
}

// duration parses a setting written the way a person writes one ("90s", "45m",
// "-1s" for never) and reports whether it said anything usable.
//
// Malformed is "said nothing", not an error, for the same reason a malformed
// port in the environment is: this is the lowest layer and it is consulted by
// every command, including the ones that connect to nothing.
func duration(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, false
	}
	return d, true
}

func applyEnv(cfg *Config) {
	if v := os.Getenv(EnvHost); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv(EnvPort); v != "" {
		// A malformed port in the environment is ignored rather than fatal:
		// it would otherwise break every command including enroll, which does
		// not connect to anything.
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
		}
	}
	if v := os.Getenv(EnvUser); v != "" {
		cfg.User = v
	}
	if v := os.Getenv(EnvEndpoint); v != "" {
		cfg.Endpoint = v
	}
	if v := os.Getenv(EnvWatch); v != "" {
		cfg.Watch = v
	}
	if v := os.Getenv(EnvWatchBudget); v != "" {
		// Ignored rather than fatal if malformed, for the same reason as the
		// port: it would otherwise break every command, including the ones
		// that connect to nothing.
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WatchBudget = n
		}
	}
	if v := os.Getenv(EnvWatchExclude); v != "" {
		cfg.WatchExclude = splitList(v)
	}
	if d, ok := duration(os.Getenv(EnvIdleTimeout)); ok {
		cfg.IdleTimeout = d
	}
	if d, ok := duration(os.Getenv(EnvDaemonIdle)); ok {
		cfg.DaemonIdle = d
	}
}

// splitList reads a comma- or os.PathListSeparator-separated list, so
// REMOTE_DOCKER_WATCH_EXCLUDE can be written either way. A semicolon list is
// what a Windows shell user reaches for; a comma list is what a Dockerfile or
// compose file does.
func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == os.PathListSeparator
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func applyOverrides(cfg *Config, o Overrides) {
	if o.Host != "" {
		cfg.Host = o.Host
	}
	if o.Port != 0 {
		cfg.Port = o.Port
	}
	if o.User != "" {
		cfg.User = o.User
	}
	if o.Endpoint != "" {
		cfg.Endpoint = o.Endpoint
	}
	if o.Watch != "" {
		cfg.Watch = o.Watch
	}
}

// EndpointFor is where this workspace's Docker API is served.
//
// Derived from the workspace name so several sessions can run at once, each
// answering on its own endpoint and each addressable as its own docker
// context. An explicitly configured endpoint always wins.
func (c Config) EndpointFor(base string) string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	if c.Name == "" || base == "" {
		// An empty base cannot be joined to. Returning it means the default,
		// which is never wrong, unlike the bare separator this used to build:
		// that named a socket relative to the working directory.
		return base
	}
	return joinEndpoint(base, sanitizeUser(c.Name))
}

// ContextName is the docker context this workspace installs.
//
// The workspace's own name, so `docker --context dev ps` reads naturally.
// Nothing else is prefixed onto it, so an install must check the context is
// one of ours before replacing it.
func (c Config) ContextName() string {
	if c.Name == "" {
		return "remote-docker"
	}
	return sanitizeUser(c.Name)
}

// RequireHost reports a usable error when no workspace is configured.
func (c Config) RequireHost() error {
	if c.Host == "" {
		return fmt.Errorf(
			"no workspace configured.\n"+
				"Set %s, pass --host, or write %s:\n"+
				"    {\"host\": \"workspace.example\", \"port\": %d, \"user\": %q}",
			EnvHost, DefaultPath(), DefaultSSHPort, c.User)
	}
	return nil
}

// DefaultPath is where the config file lives.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".remote-docker.json"
	}
	return filepath.Join(home, ".remote-docker.json")
}

// StateDir is where keys, known_hosts and session state are kept.
func StateDir() string {
	if dir := os.Getenv("REMOTE_DOCKER_STATE_DIR"); dir != "" {
		return dir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "remote-docker")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "remote-docker")
	}
	return filepath.Join(home, ".remote-docker")
}

// KeyPath is this machine's private key.
func KeyPath() string { return filepath.Join(StateDir(), "id_ed25519") }

// KnownHostsPath records workspace host keys.
func KnownHostsPath() string { return filepath.Join(StateDir(), "known_hosts") }

// KeyComment identifies this machine on the key it generates.
//
// It is the only thing distinguishing one file from another in a workspace's
// authorized_keys.d, so whoever enrols a key can tell whose machine it came
// from. Lives here rather than in session because enroll needs it too, and the
// two disagreeing meant the comment depended on which command happened to
// generate the key.
func KeyComment() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	user := ""
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			user = v
			break
		}
	}
	if user == "" {
		return "remote-docker-" + host
	}
	return "remote-docker-" + host + "-" + user
}

// defaultUser guesses the workspace account from the local username, because
// the enrolled .pub file is usually named after it.
func defaultUser() string {
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return sanitizeUser(v)
		}
	}
	return "user"
}

// sanitizeUser reduces a local username to what the workspace will accept as
// an account name, matching how the server derives one from a .pub filename.
func sanitizeUser(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.TrimLeft(b.String(), "0123456789-")
	if out == "" {
		return "user"
	}
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

// Save writes the config file, creating its directory if needed.
//
// Written to a temporary file in the same directory and renamed over the
// original, so an interrupted write leaves the previous config intact rather
// than a truncated one. This file is the only record of how to reach a
// workspace; half of it is worse than none.
func Save(file File, path string) error {
	if path == "" {
		path = DefaultPath()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("config: creating %s: %w", dir, err)
		}
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".remote-docker-*.json")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// Renamed straight over the old file, never unlinked first.
	//
	// This used to `os.Remove(path)` before renaming, for a stated reason
	// ("Windows will not rename onto an existing file") that is not true of
	// os.Rename: it calls MoveFileEx with MOVEFILE_REPLACE_EXISTING, which
	// replaces. Measured on Windows rather than assumed, because the comment
	// was confident and wrong.
	//
	// What it cost: between the Remove and the Rename the config file DOES NOT
	// EXIST, and Load treats a missing file as an empty config with no error --
	// so a `remote-docker workspace ls` that read in that window printed
	// nothing and exited 0, having been told there were no workspaces. It
	// showed up as one flaky integration assertion; the same window is open to
	// anything else reading the file while a session writes it.
	//
	// The rename is RETRIED rather than forced. On Windows it fails with a
	// sharing violation while another process has the file open, usually a
	// reader for the few microseconds it takes, which is transient.
	// Unlinking to make room is what the old code did, and it is
	// the bug: it trades a brief failure for a brief absence, and an absent
	// config reads as an empty one rather than as a problem.
	//
	// Measured, not assumed: with the unlink in place the test beside this
	// sees an empty config within a couple of hundred iterations; with an
	// unlinking FALLBACK it still does, which is how this ended up a retry.
	for attempt := range renameRetries {
		if err = os.Rename(tmpName, path); err == nil {
			return nil
		}
		if attempt < renameRetries-1 {
			time.Sleep(readBackoff)
		}
	}
	// Reported rather than forced. A save that fails is recoverable: the old
	// config is intact and the user is told. A config file that disappeared is
	// not.
	return fmt.Errorf("config: replacing %s: %w", path, err)
}

// renameRetries bounds how long Save waits for a reader to let go. Generous
// enough to outlast reading a small JSON file many times over.
const renameRetries = 10

// Set adds or replaces a workspace.
//
// A file written by hand may describe a single workspace with no name, in the
// flat form. Adding a second one has to move the first into the keyed form,
// or it would be shadowed by the top-level fields and silently unreachable.
func (f *File) Set(name string, ws Workspace) error {
	if name == "" {
		return fmt.Errorf("config: a workspace needs a name")
	}
	if f.Workspaces == nil {
		f.Workspaces = map[string]Workspace{}
	}
	if f.Host != "" {
		existing := Workspace{Host: f.Host, Port: f.Port, User: f.User, Endpoint: f.Endpoint}
		flat := f.Default
		if flat == "" {
			flat = f.Host
		}
		if flat != name {
			if _, taken := f.Workspaces[flat]; !taken {
				f.Workspaces[flat] = existing
			}
		}
		f.Host, f.Port, f.User, f.Endpoint = "", 0, "", ""
	}
	f.Workspaces[name] = ws
	if f.Default == "" {
		f.Default = name
	}
	return nil
}

// Remove deletes a workspace. It reports whether there was one to delete.
func (f *File) Remove(name string) bool {
	if _, ok := f.Workspaces[name]; !ok {
		return false
	}
	delete(f.Workspaces, name)
	if f.Default != name {
		return true
	}
	// The default pointed at what was just removed. Promote the only
	// remaining workspace if there is exactly one, because then the choice is
	// unambiguous; otherwise leave it unset rather than picking for the user.
	f.Default = ""
	if names := f.Names(); len(names) == 1 {
		f.Default = names[0]
	}
	return true
}
