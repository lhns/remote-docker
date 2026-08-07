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
	"strconv"
	"strings"
)

// DefaultSSHPort is the workspace's sshd port.
const DefaultSSHPort = 2222

// Config is everything needed to open a session.
type Config struct {
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
}

// File is the on-disk form, ~/.remote-docker.json.
type File struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	User     string `json:"user,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// Overrides are values supplied on the command line. Zero values mean
// "not specified" and fall through to the next source.
type Overrides struct {
	Host     string
	Port     int
	User     string
	Endpoint string
}

// Environment variable names.
const (
	EnvHost     = "REMOTE_DOCKER_HOST"
	EnvPort     = "REMOTE_DOCKER_PORT"
	EnvUser     = "REMOTE_DOCKER_USER"
	EnvEndpoint = "REMOTE_DOCKER_ENDPOINT"
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
	applyFile(&cfg, file)
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
