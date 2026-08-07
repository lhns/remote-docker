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
}

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
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	User         string   `json:"user,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Watch        string   `json:"watch,omitempty"`
	WatchBudget  int      `json:"watchBudget,omitempty"`
	WatchExclude []string `json:"watchExclude,omitempty"`

	Workspaces map[string]Workspace `json:"workspaces,omitempty"`
	Default    string               `json:"default,omitempty"`
}

// Workspace is one entry in the keyed form.
type Workspace struct {
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	User         string   `json:"user,omitempty"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Watch        string   `json:"watch,omitempty"`
	WatchBudget  int      `json:"watchBudget,omitempty"`
	WatchExclude []string `json:"watchExclude,omitempty"`
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
// is exactly one, that one -- because with a single workspace configured, not
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
)

// Resolve combines the sources in order of decreasing precedence: command
// line, environment, config file, defaults.
//
// The file is optional and a missing one is not an error -- `enroll` has to
// work before anything is configured, since that is how a key gets issued in
// the first place.
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

	applyFile(&cfg, file)
	applyWorkspace(&cfg, ws)
	applyEnv(&cfg)
	applyOverrides(&cfg, o)

	if cfg.Port < 1 || cfg.Port > 65535 {
		return Config{}, fmt.Errorf("config: port %d is not valid", cfg.Port)
	}
	return cfg, nil
}

// Load reads the config file. A missing file yields a zero File and no error.
func Load(path string) (File, error) {
	if path == "" {
		path = DefaultPath()
	}
	data, err := os.ReadFile(path)
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

func applyFile(cfg *Config, file File) {
	if file.Host != "" {
		cfg.Host = file.Host
	}
	if file.Port != 0 {
		cfg.Port = file.Port
	}
	if file.User != "" {
		cfg.User = file.User
	}
	if file.Endpoint != "" {
		cfg.Endpoint = file.Endpoint
	}
	if file.Watch != "" {
		cfg.Watch = file.Watch
	}
	if file.WatchBudget != 0 {
		cfg.WatchBudget = file.WatchBudget
	}
	if len(file.WatchExclude) > 0 {
		cfg.WatchExclude = file.WatchExclude
	}
}

// applyWorkspace overlays a named workspace on top of the file's flat fields,
// so shared settings can sit at the top level and be specialised per entry.
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
}

// splitList reads a comma- or os.PathListSeparator-separated list, so
// REMOTE_DOCKER_WATCH_EXCLUDE can be written either way -- a semicolon list on
// Windows is what a shell user reaches for, and a comma list is what a
// Dockerfile or compose file does.
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
	if c.Name == "" {
		return base
	}
	return base + endpointSeparator + sanitizeUser(c.Name)
}

// ContextName is the docker context this workspace installs.
//
// The workspace's own name, so `docker --context dev ps` reads naturally.
// Nothing else is prefixed onto it, which means an install must check the
// context is one of ours before replacing it -- see the install command.
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
	// Windows will not rename onto an existing file.
	_ = os.Remove(path)
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: replacing %s: %w", path, err)
	}
	return nil
}

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
