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

	"github.com/lhns/remote-docker/core/workspace"
)

// DefaultSSHPort is the workspace's sshd port.
const DefaultSSHPort = 2222

// Config is everything needed to open a session.
type Config struct {
	// Name identifies the workspace among several; empty for a flat file.
	Name string

	Host string

	// Port is the SSH port. A host with a scheme may carry it instead, and
	// Transport refuses the two disagreeing.
	Port int

	// CAFile verifies a ws:// endpoint's proxy against a private CA.
	CAFile string

	// Insecure accepts any certificate from a ws:// endpoint's proxy. SSH
	// inside still authenticates both ends. Per workspace, never global.
	Insecure bool

	// Machine is the local machine this workspace runs on, or nil. Its
	// address is assigned at boot, so it is located on every connect.
	Machine *Machine

	// User is the workspace account, also the name of the enrolled .pub file.
	User string

	// Endpoint is where the local Docker API is served; empty is the default.
	Endpoint string

	// Watch is "off", "partial" or "coarse" (ADR 0016). This and the other
	// mode strings stay raw: the command parses and reports a bad value.
	Watch string

	// WatchBudget caps directories watched at once; zero is the platform
	// default.
	WatchBudget int

	// CacheFiles and CacheBytes cap a delegated share's cache fill; zero is
	// dircache's default. What does not fit is served live (ADR 0044).
	CacheFiles int
	CacheBytes int64

	// Prefetch is "off", "eager" or "tree" (ADR 0045).
	Prefetch string

	// WatchExclude replaces the default list of directory names never watched.
	WatchExclude []string

	// Consistency fills the axes a mount left unset (ADR 0042); unset is
	// read=direct,write=through. ConsistencyPaths overrides it per directory
	// tree.
	Consistency      string
	ConsistencyPaths map[string]string

	// IdleTimeout releases an unused workspace connection (ADR 0015). Zero is
	// the default; negative never releases.
	IdleTimeout time.Duration

	// DaemonIdle ends an unused background session. Zero is DefaultDaemonIdle;
	// negative never. The exit takes the endpoint, which every other Docker
	// client pointed at it then cannot reach.
	DaemonIdle time.Duration

	// DaemonStandby drops the connection and file watches of an unused
	// background session while keeping the endpoint bound. Zero is
	// DefaultDaemonStandby; negative never.
	DaemonStandby time.Duration
}

// DefaultDaemonIdle is never: exiting takes the endpoint that compose, buildx
// and IDEs connect to, and only a remote-docker command can bring it back.
const DefaultDaemonIdle = DaemonIdleNever

// DaemonIdleNever is any non-positive duration; idleExpired treats <= 0 as
// "no reclaim".
const DaemonIdleNever = -1 * time.Second

// DefaultDaemonStandby is how long a session holds the workspace unused.
const DefaultDaemonStandby = 30 * time.Minute

// File is the on-disk form, ~/.remote-docker.json, flat for one workspace or
// keyed for several:
//
//	{"host": "dev.example", "user": "alice"}
//	{"workspaces": {"dev": {...}, "ci": {...}}, "default": "dev"}
type File struct {
	// Embedded, so the flat form's fields are a Workspace's.
	Workspace

	Workspaces map[string]Workspace `json:"workspaces,omitempty"`
	Default    string               `json:"default,omitempty"`
}

// Workspace is one entry in the keyed form. Durations are strings ("90s"),
// parsed in applyWorkspace, where a malformed one is ignored.
type Workspace struct {
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	CAFile      string `json:"caFile,omitempty"`
	Insecure    bool   `json:"insecure,omitempty"`
	User        string `json:"user,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Watch       string `json:"watch,omitempty"`
	Consistency string `json:"consistency,omitempty"`

	// Keyed by a local path; applies to everything under it.
	ConsistencyPaths map[string]string `json:"consistencyPaths,omitempty"`

	WatchBudget   int      `json:"watchBudget,omitempty"`
	CacheFiles    int      `json:"cacheFiles,omitempty"`
	CacheBytes    int64    `json:"cacheBytes,omitempty"`
	Prefetch      string   `json:"prefetch,omitempty"`
	WatchExclude  []string `json:"watchExclude,omitempty"`
	IdleTimeout   string   `json:"idleTimeout,omitempty"`
	DaemonIdle    string   `json:"daemonIdle,omitempty"`
	DaemonStandby string   `json:"daemonStandby,omitempty"`

	// Machine is set when this program provisioned the workspace's Linux
	// system. It is the only record one was built, which is why `rm` refuses
	// to drop the entry without destroying the machine (ADR 0026).
	Machine *Machine `json:"machine,omitempty"`
}

// Machine records the inputs of a build, never the machine's current state,
// which is always asked of the backend.
type Machine struct {
	// Backend is "wsl" or "hyperv".
	Backend string `json:"backend"`

	// Name is the machine's name on the host, not the workspace's.
	Name string `json:"name"`

	// Image is the workspace image, by full pinned reference.
	Image string `json:"image,omitempty"`

	// Rootfs is where the image's filesystem came from, for an identical
	// rebuild.
	Rootfs string `json:"rootfs,omitempty"`

	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memoryMb,omitempty"`

	// Generation hashes the build settings; a mismatch means out of date.
	Generation string `json:"generation,omitempty"`
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

// selected picks which workspace a request refers to: the name given, else
// the file's default, else the only one.
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
}

// Environment variable names.
const (
	EnvHost      = "REMOTE_DOCKER_HOST"
	EnvPort      = "REMOTE_DOCKER_PORT"
	EnvUser      = "REMOTE_DOCKER_USER"
	EnvEndpoint  = "REMOTE_DOCKER_ENDPOINT"
	EnvWorkspace = "REMOTE_DOCKER_WORKSPACE"

	EnvCAFile   = "REMOTE_DOCKER_CA_FILE"
	EnvInsecure = "REMOTE_DOCKER_INSECURE"

	EnvConsistency   = "REMOTE_DOCKER_CONSISTENCY"
	EnvWatch         = "REMOTE_DOCKER_WATCH"
	EnvWatchBudget   = "REMOTE_DOCKER_WATCH_BUDGET"
	EnvCacheFiles    = "REMOTE_DOCKER_CACHE_FILES"
	EnvCacheBytes    = "REMOTE_DOCKER_CACHE_BYTES"
	EnvPrefetch      = "REMOTE_DOCKER_PREFETCH"
	EnvWatchExclude  = "REMOTE_DOCKER_WATCH_EXCLUDE"
	EnvIdleTimeout   = "REMOTE_DOCKER_IDLE_TIMEOUT"
	EnvDaemonIdle    = "REMOTE_DOCKER_DAEMON_IDLE"
	EnvDaemonStandby = "REMOTE_DOCKER_DAEMON_STANDBY"
)

// Resolve combines the sources in order of decreasing precedence: command
// line, environment, config file, defaults.
//
// The file is optional and a missing one is not an error: `enroll` has to work
// before anything is configured, since that is how a key gets issued.
func Resolve(o Overrides, path string) (Config, error) {
	cfg := Config{User: DefaultUser()}

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

	// Defaulted only for a bare host. A host with a scheme says its own port or
	// gets its scheme's default from Transport, and a 2222 put here would be
	// indistinguishable from a `port` somebody set: wss:// would inherit it
	// instead of 443, and ssh://host:2299 would be refused as contradicting it.
	if _, _, scheme := splitScheme(strings.TrimSpace(cfg.Host)); cfg.Port == 0 && !scheme {
		cfg.Port = DefaultSSHPort
	}
	// Zero is "not set", which Transport resolves from the scheme.
	if cfg.Port != 0 && (cfg.Port < 0 || cfg.Port > 65535) {
		return Config{}, fmt.Errorf("config: port %d is not valid", cfg.Port)
	}
	return cfg, nil
}

// A read is retried because on Windows a concurrent Save's rename can refuse
// an opener with a transient sharing violation. A missing file is not retried.
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

// applyWorkspace overlays the settings a workspace sets. Applied to the flat
// fields, then to the named entry on top.
func applyWorkspace(cfg *Config, ws Workspace) {
	if ws.Host != "" {
		cfg.Host = ws.Host
	}
	if ws.Port != 0 {
		cfg.Port = ws.Port
	}
	if ws.Machine != nil {
		cfg.Machine = ws.Machine
	}
	if ws.User != "" {
		cfg.User = ws.User
	}
	if ws.Endpoint != "" {
		cfg.Endpoint = ws.Endpoint
	}
	if ws.CAFile != "" {
		cfg.CAFile = ws.CAFile
	}
	if ws.Insecure {
		cfg.Insecure = true
	}
	if ws.Watch != "" {
		cfg.Watch = ws.Watch
	}
	if ws.Consistency != "" {
		cfg.Consistency = ws.Consistency
	}
	if len(ws.ConsistencyPaths) > 0 {
		cfg.ConsistencyPaths = ws.ConsistencyPaths
	}
	if ws.CacheFiles != 0 {
		cfg.CacheFiles = ws.CacheFiles
	}
	if ws.CacheBytes != 0 {
		cfg.CacheBytes = ws.CacheBytes
	}
	if ws.Prefetch != "" {
		cfg.Prefetch = ws.Prefetch
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
	if d, ok := duration(ws.DaemonStandby); ok {
		cfg.DaemonStandby = d
	}
	if d, ok := duration(ws.DaemonIdle); ok {
		cfg.DaemonIdle = d
	}
}

// duration parses "90s", "-1s" and the like. Malformed means unset, never an
// error: every command reads config, including ones that connect to nothing.
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
		// Malformed numbers here are ignored, as in duration.
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
	if v := os.Getenv(EnvCAFile); v != "" {
		cfg.CAFile = v
	}
	if v := os.Getenv(EnvInsecure); v != "" {
		// Anything but "0" or "false" turns it on.
		cfg.Insecure = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv(EnvWatch); v != "" {
		cfg.Watch = v
	}
	if v := os.Getenv(EnvConsistency); v != "" {
		cfg.Consistency = v
	}
	if v := os.Getenv(EnvWatchBudget); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WatchBudget = n
		}
	}
	if v := os.Getenv(EnvCacheFiles); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CacheFiles = n
		}
	}
	if v := os.Getenv(EnvCacheBytes); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.CacheBytes = n
		}
	}
	if v := os.Getenv(EnvPrefetch); v != "" {
		cfg.Prefetch = v
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
	if d, ok := duration(os.Getenv(EnvDaemonStandby)); ok {
		cfg.DaemonStandby = d
	}
}

// splitList reads a comma- or os.PathListSeparator-separated list.
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
}

// EndpointFor is where this workspace's Docker API is served: the configured
// endpoint, else base suffixed with the workspace name.
func (c Config) EndpointFor(base string) string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	if c.Name == "" || base == "" {
		// Suffixing "" would name a relative path.
		return base
	}
	return joinEndpoint(base, sanitizeUser(c.Name))
}

// ContextName is the docker context this workspace installs: its bare name,
// so an install must check a context is ours before replacing it.
func (c Config) ContextName() string {
	if c.Name == "" {
		return "remote-docker"
	}
	return sanitizeUser(c.Name)
}

// ErrNoWorkspace is RequireHost's answer. Its remedy names one of our
// commands, which only the caller can spell, so the caller adds it.
var ErrNoWorkspace = errors.New("no workspace configured")

// RequireHost reports ErrNoWorkspace when no workspace is configured.
func (c Config) RequireHost() error {
	if c.Host == "" {
		return ErrNoWorkspace
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

// SharesPath records which local directories a workspace has exported. Per
// workspace, so one workspace's record never answers another's mount.
func SharesPath(workspace string) string {
	if workspace == "" {
		workspace = "default"
	}
	return filepath.Join(StateDir(), "shares", workspace+".json")
}

// CachedPath records which files a delegated share's cache was filled with,
// per workspace like SharesPath.
func CachedPath(workspace string) string {
	if workspace == "" {
		workspace = "default"
	}
	return filepath.Join(StateDir(), "caches", workspace+".json")
}

// KeyComment identifies this machine on the key it generates, so whoever
// enrols it can tell whose machine it came from.
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

// DefaultUser guesses the workspace account from the local username, because
// the enrolled .pub file is usually named after it.
func DefaultUser() string {
	for _, key := range []string{"USER", "USERNAME", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return sanitizeUser(v)
		}
	}
	return "user"
}

// sanitizeUser reduces a name to a workspace account name, by the workspace's
// own rule, or "user" if nothing can be derived.
func sanitizeUser(name string) string {
	account, err := workspace.AccountName(name)
	if err != nil {
		return "user"
	}
	return account
}

// Save writes the config file atomically, creating its directory if needed.
func Save(file File, path string) error {
	if path == "" {
		path = DefaultPath()
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding: %w", err)
	}
	data = append(data, '\n')

	return WriteAtomic(path, data, 0)
}

// WriteAtomic replaces a file with new contents, or leaves it as it was. A
// mode of 0 keeps the temporary file's.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("config: creating %s: %w", dir, err)
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".remote-docker-*")
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
	if mode != 0 {
		if err := os.Chmod(tmpName, mode); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	// Rename over the old file, NEVER unlink first: Load reads a missing file
	// as an empty config with no error. A sharing violation from a reader is
	// retried instead, and finally reported with the old file intact.
	for attempt := range renameRetries {
		if err = os.Rename(tmpName, path); err == nil {
			return nil
		}
		if attempt < renameRetries-1 {
			time.Sleep(readBackoff)
		}
	}
	return fmt.Errorf("config: replacing %s: %w", path, err)
}

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
	// Only while there are no keyed entries is the flat form a workspace of
	// its own. Beside keyed entries it is the base applyWorkspace lays under
	// each of them, and moving it would take it away from every one.
	if f.Host != "" && len(f.Workspaces) == 0 {
		// The WHOLE entry moves, not the four fields that name the address.
		// Its watch mode, its consistency rules and above all its `machine`
		// belong to that workspace; left at the top level they become a base,
		// and the new workspace silently inherits a machine it does not have.
		flat := f.Default
		if flat == "" {
			flat = f.Host
		}
		if flat != name {
			f.Workspaces = map[string]Workspace{flat: f.Workspace}
		}
		f.Workspace = Workspace{}
	}
	if f.Workspaces == nil {
		f.Workspaces = map[string]Workspace{}
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
	// Promote the only remaining workspace; never pick among several.
	f.Default = ""
	if names := f.Names(); len(names) == 1 {
		f.Default = names[0]
	}
	return true
}
