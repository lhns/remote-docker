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

	// Port is the workspace's SSH port. Optional: a host carrying a scheme and
	// a port says it there instead, and Transport refuses the two disagreeing.
	Port int

	// CAFile verifies a ws:// endpoint's proxy against a private CA instead of
	// the system roots. Ignored for ssh, which has its own known_hosts.
	CAFile string

	// Insecure accepts any certificate from a ws:// endpoint's proxy. It gives
	// up knowing WHICH proxy answered and nothing else: SSH inside the tunnel
	// still authenticates both ends. Per workspace, never global.
	Insecure bool

	// Machine is the local machine this workspace runs on, or nil for a
	// workspace somewhere else.
	//
	// Carried this far because a machine has to be located before it can be
	// dialled: it is started on demand and its address is given to it at boot,
	// so Host is not the answer for one and a stored address goes stale when it
	// restarts.
	Machine *Machine

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

	// CacheFiles and CacheBytes cap what a delegated share's cache is filled
	// with. Zero means dircache's defaults.
	//
	// A ceiling rather than a refusal: what the fill does not copy is served
	// from the live export underneath, so a project over it is cached in part
	// and works (ADR 0044). Raise them for a large repository whose reads are
	// worth the copy; lower them on a metered link.
	CacheFiles int
	CacheBytes int64

	// Prefetch is whether a union with read=cached is filled ahead of reads,
	// and how: "off" (the default), "eager" (the whole tree smallest first)
	// or "tree" (what is read, and its neighbourhood) (ADR 0045).
	Prefetch string

	// WatchExclude replaces the default list of directory names never
	// watched. Empty means the default.
	WatchExclude []string

	// Consistency is what a share's mount gets on each axis the mount itself
	// left unset: `read=<direct|cached>,write=<through|back|ephemeral>`, with
	// Docker's `consistent`, `cached` and `delegated` accepted as aliases
	// (ADR 0042). Unset is read=direct,write=through.
	//
	// ConsistencyPaths overrides it for one directory and everything under it,
	// which is the common case of one slow tree among fast ones.
	//
	// Both held as raw strings for the same reason as Watch: this package is
	// the lowest layer and the command reports a bad value.
	Consistency      string
	ConsistencyPaths map[string]string

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
	//
	// Setting it is a choice, not a tuning knob: the exit takes the ENDPOINT,
	// and every other Docker client pointed at it then fails with ENOENT and
	// cannot recover.
	DaemonIdle time.Duration

	// DaemonStandby is how long a background session may sit with nothing to
	// do before it lets go of the workspace: the connection is dropped and the
	// file watches are released. Zero uses DefaultDaemonStandby; negative
	// never stands by.
	//
	// The endpoint stays bound throughout, so this reclaims what a reclaim is
	// for without breaking the Docker clients pointed at it. DaemonIdle is the
	// tier above, and ends the process.
	DaemonStandby time.Duration
}

// DefaultDaemonIdle is how long a background session outlives its last use.
//
// Never, because the reclaim takes the ENDPOINT with it, and the endpoint is
// what compose, buildx, Testcontainers and IDE plugins connect to. They know
// nothing of sessions, so only a remote-docker command can rebuild one.
//
// It reclaims little in return: Session.sweepIdle already releases the
// connection on its own timer and reopens it per request, invisibly.
//
// Still honoured when set, which is reasonable on a laptop.
const DefaultDaemonIdle = DaemonIdleNever

// DaemonIdleNever is any non-positive duration; idleExpired treats <= 0 as
// "no reclaim". Spelled once so the default and the config agree.
const DaemonIdleNever = -1 * time.Second

// DefaultDaemonStandby is how long a session holds the workspace unused.
//
// Half an hour, which is what DaemonIdle used to be: long enough that stepping
// away and coming back finds it warm, short enough that a workspace opened once
// last week is not still holding a connection and a few thousand watches.
const DefaultDaemonStandby = 30 * time.Minute

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
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port,omitempty"`
	CAFile      string `json:"caFile,omitempty"`
	Insecure    bool   `json:"insecure,omitempty"`
	User        string `json:"user,omitempty"`
	Endpoint    string `json:"endpoint,omitempty"`
	Watch       string `json:"watch,omitempty"`
	Consistency string `json:"consistency,omitempty"`

	// Keyed by a path on this machine; the value applies to it and to
	// everything under it.
	ConsistencyPaths map[string]string `json:"consistencyPaths,omitempty"`

	WatchBudget   int      `json:"watchBudget,omitempty"`
	CacheFiles    int      `json:"cacheFiles,omitempty"`
	CacheBytes    int64    `json:"cacheBytes,omitempty"`
	Prefetch      string   `json:"prefetch,omitempty"`
	WatchExclude  []string `json:"watchExclude,omitempty"`
	IdleTimeout   string   `json:"idleTimeout,omitempty"`
	DaemonIdle    string   `json:"daemonIdle,omitempty"`
	DaemonStandby string   `json:"daemonStandby,omitempty"`

	// Machine is set when this program provisioned the Linux system the
	// workspace runs on, rather than being pointed at one that already
	// existed.
	//
	// Its presence is what makes a machine-backed workspace an ordinary
	// workspace everywhere else: `ls` lists it, `use` selects it, a session
	// reaches it over SSH like any other. The one place it changes anything is
	// `rm`, which has a machine to destroy as well as an entry to delete --
	// leaving that behind would strand a running Linux system on somebody's
	// laptop with nothing in the config naming it.
	Machine *Machine `json:"machine,omitempty"`
}

// Machine records what this program built, so that it can be recognised,
// rebuilt identically, and taken away again.
//
// Everything here is an input to the build. Nothing describes the machine's
// current state: that is asked of the backend, because a cached answer about a
// thing the user can stop from outside this program is a lie waiting to
// happen.
type Machine struct {
	// Backend is "wsl" or "hyperv".
	Backend string `json:"backend"`

	// Name is what the machine is called on the host, which is not the
	// workspace's name.
	Name string `json:"name"`

	// Image is the workspace image it runs, by full reference. Pinned rather
	// than floating: a machine that quietly became a different version on
	// restart is the failure this whole design is arranged to avoid.
	Image string `json:"image,omitempty"`

	// Rootfs is where that image's filesystem came from, so a rebuild starts
	// from the same one rather than from whatever is current.
	Rootfs string `json:"rootfs,omitempty"`

	CPUs     int `json:"cpus,omitempty"`
	MemoryMB int `json:"memoryMb,omitempty"`

	// Generation is the hash of the settings it was built from. A mismatch
	// against the current ones is how "this machine is out of date" is known
	// without inspecting it.
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
	CAFile    string
	Insecure  bool
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

	// The SSH port is defaulted only once the host is known, and only when the
	// host is not a WebSocket. Defaulting it up front would make 2222
	// indistinguishable from a port somebody asked for, and every wss://
	// workspace would then inherit the SSH port instead of 443.
	if cfg.Port == 0 && !isWebSocketHost(cfg.Host) {
		cfg.Port = DefaultSSHPort
	}
	// Zero is "not set", which Transport resolves from the scheme.
	if cfg.Port != 0 && (cfg.Port < 0 || cfg.Port > 65535) {
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
// entry. One function applied twice rather than two with the same clauses --
// two would let the levels disagree about what overrides what.
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
	if v := os.Getenv(EnvCAFile); v != "" {
		cfg.CAFile = v
	}
	if v := os.Getenv(EnvInsecure); v != "" {
		// Anything but an explicit falsehood turns it on: this is a switch, and
		// somebody who set it to "yes" meant yes.
		cfg.Insecure = v != "0" && !strings.EqualFold(v, "false")
	}
	if v := os.Getenv(EnvWatch); v != "" {
		cfg.Watch = v
	}
	if v := os.Getenv(EnvConsistency); v != "" {
		cfg.Consistency = v
	}
	if v := os.Getenv(EnvWatchBudget); v != "" {
		// Ignored rather than fatal if malformed, for the same reason as the
		// port: it would otherwise break every command, including the ones
		// that connect to nothing.
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
	if o.CAFile != "" {
		cfg.CAFile = o.CAFile
	}
	if o.Insecure {
		cfg.Insecure = true
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

// SharesPath records which local directories a workspace has been asked to
// export.
//
// Per workspace, because the volumes naming those exports live on that
// workspace's daemon, and one workspace's record must never answer another's
// mount.
func SharesPath(workspace string) string {
	if workspace == "" {
		workspace = "default"
	}
	return filepath.Join(StateDir(), "shares", workspace+".json")
}

// CachedPath records which files a delegated share's cache was filled with.
//
// Per workspace for the same reason SharesPath is: a cache lives on one
// workspace's daemon, and another's record must never decide what to remove
// from it.
func CachedPath(workspace string) string {
	if workspace == "" {
		workspace = "default"
	}
	return filepath.Join(StateDir(), "caches", workspace+".json")
}

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

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding: %w", err)
	}
	data = append(data, '\n')

	return WriteAtomic(path, data, 0)
}

// WriteAtomic replaces a file with new contents, or leaves it as it was.
//
// Shared rather than copied, because what follows is the part that is easy to
// get subtly wrong and this client writes more than one file that must never
// be read half-written. A mode of 0 keeps whatever the temporary file had,
// which is what the config wants; a file naming local directories asks for
// 0o600.
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
	// Renamed straight over the old file, NEVER unlinked first.
	//
	// Unlinking opens a window where the config does not exist, and Load reads
	// a missing file as an empty config with no error: `workspace ls` in that
	// window prints nothing and exits 0. os.Rename does not need the unlink
	// anyway, since MoveFileEx replaces on Windows too.
	//
	// Retried rather than forced. A rename can fail with a sharing violation
	// while a reader has the file open, which is transient and loud; unlinking
	// to make room trades that for a brief absence, which is silent. The test
	// beside this catches an unlinking version within a couple of hundred
	// iterations, fallback included.
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
