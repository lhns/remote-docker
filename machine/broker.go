package machine

// The Hyper-V broker: what a standard user and the SYSTEM service say to each
// other (ADR 0055).
//
// Managing Hyper-V takes administrator rights, so a service running as SYSTEM
// does it for the people allowed to ask. This file is the agreement: the names,
// the typed operations and what is accepted in them. The service side is
// broker_server.go and the asking side is broker_client.go; neither takes a
// path, a script or a command from the other.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// BrokerServiceName is the Windows service.
	BrokerServiceName = "remote-docker-machines"

	// BrokerGroup is the local group whose DIRECT members may use the service.
	// Nested and domain groups are not expanded.
	BrokerGroup = "remote-docker-users"

	// brokerBackend is the registered name of the Hyper-V backend that goes
	// through the service. A machine's config says `broker: true` and
	// Spec.BackendKey turns that into this.
	brokerBackend = "hyperv-broker"
)

// brokerPipeSDDL: SYSTEM and Administrators full, Authenticated Users read and
// write WITHOUT creating a pipe instance, and network logons denied first. The
// mask is FILE_GENERIC_READ | FILE_GENERIC_WRITE minus 0x4: on a pipe,
// FILE_APPEND_DATA is FILE_CREATE_PIPE_INSTANCE, and a user who could make a
// second instance would be handed connections meant for the service. A named pipe is also reachable over
// SMB, and the service is for people sitting at this computer. Who may DO
// anything is decided inside, after the caller is identified.
const brokerPipeSDDL = "D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;AU)"

// brokerDirSDDL is the DACL of the service's directory in ProgramData:
// protected, so nothing is inherited from a parent where Users may create
// things, and SYSTEM and Administrators only.
const brokerDirSDDL = "D:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// The operations. Typed, each naming one machine; none carries a path or a
// script.
const (
	opAvailable = "available"
	opLook      = "look"
	opCreate    = "create"
	opStart     = "start"
	opStop      = "stop"
	opAddress   = "address"
	opRetract   = "retract"
	opDestroy   = "destroy"
	// opPurge destroys every machine the service owns. SYSTEM and
	// Administrators only; it is what uninstalling does.
	opPurge = "purge"
)

// Error codes in a BrokerResponse.
const (
	codeNotAllowed = "not-allowed" // the caller may not use the service
	codeNotYours   = "not-yours"   // the machine is recorded for someone else
	codeTaken      = "taken"       // create: the name is in use
	codeInvalid    = "invalid"     // a malformed request
	codeFailed     = "failed"      // Hyper-V said no
)

// BrokerRequest is one operation. The caller is never in it: the service reads
// that off the pipe.
type BrokerRequest struct {
	Op     string        `json:"op"`
	Name   string        `json:"name,omitempty"`
	Create *BrokerCreate `json:"create,omitempty"`
}

// BrokerCreate is everything create takes. The service fetches the disk and
// builds the Ignition document itself.
type BrokerCreate struct {
	PublicKey string `json:"publicKey"`
	// HostKey is the agent's private host key, as PEM, built in at creation.
	HostKey  string `json:"hostKey,omitempty"`
	Account  string `json:"account"`
	Port     int    `json:"port"`
	CPUs     int    `json:"cpus,omitempty"`
	MemoryMB int    `json:"memoryMb,omitempty"`
	Image    string `json:"image"`
	Proxy    string `json:"proxy,omitempty"`
}

// BrokerNotes is hyperVNotes on the wire.
type BrokerNotes struct {
	Generation string `json:"generation,omitempty"`
	Key        string `json:"key,omitempty"`
}

// BrokerResponse is the answer to one request.
type BrokerResponse struct {
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`

	State   string       `json:"state,omitempty"` // look: absent, stopped or running
	Notes   *BrokerNotes `json:"notes,omitempty"` // look
	Address string       `json:"address,omitempty"`
	Purged  []string     `json:"purged,omitempty"` // purge
}

// BrokerError is a refusal or failure the service reported.
type BrokerError struct{ Code, Message string }

func (e *BrokerError) Error() string {
	if e.Code == codeNotAllowed {
		return "this account may not use the Hyper-V service\n" +
			"  fix: ask an administrator to add you to " + BrokerGroup +
			" (net localgroup " + BrokerGroup + " <you> /add)"
	}
	return e.Message
}

var (
	brokerNameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	brokerAccountRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,29}$`)
	brokerImageRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)
)

// validBrokerName holds a machine name to what becomes a directory name and a
// VM name: nothing that is a path.
func validBrokerName(name string) error {
	if !brokerNameRE.MatchString(name) {
		return fmt.Errorf("%q is not a valid machine name: lower-case letters, digits and hyphens, starting with a letter or digit, at most 63 characters", name)
	}
	return nil
}

// validate checks a create request. Everything in it ends up in a systemd unit,
// an Ignition document or a PowerShell parameter, so each field is held to the
// narrowest shape it legitimately has.
func (c BrokerCreate) validate() error {
	switch {
	case !brokerAccountRE.MatchString(c.Account):
		return errors.New("account is not a valid unix account name")
	case !brokerImageRE.MatchString(c.Image):
		return errors.New("image is not a valid image reference")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("port is out of range")
	case c.CPUs < 0 || c.CPUs > 256:
		return errors.New("cpus is out of range")
	case c.MemoryMB < 0 || c.MemoryMB > 1<<20:
		return errors.New("memory is out of range")
	}
	key := strings.TrimSpace(c.PublicKey)
	if len(key) > 4096 || strings.ContainsAny(key, "\r\n\x00") || len(strings.Fields(key)) < 2 {
		return errors.New("public key is not a single authorized_keys line")
	}
	if c.HostKey != "" && (len(c.HostKey) > 16384 || !strings.HasPrefix(c.HostKey, "-----BEGIN ")) {
		return errors.New("host key is not a PEM private key")
	}
	if c.Proxy != "" {
		if _, err := ParseProxy(c.Proxy); err != nil {
			return err
		}
	}
	return nil
}

// spec is the Spec the service builds a machine from. Rootfs is the disk the
// service itself fetched.
func (c BrokerCreate) spec(name, rootfs string) Spec {
	return Spec{
		Name: name, Backend: "hyperv", Image: c.Image,
		Rootfs: rootfs, Fetched: true,
		CPUs: c.CPUs, MemoryMB: c.MemoryMB, Port: c.Port, Account: c.Account,
		PublicKey: c.PublicKey, Proxy: c.Proxy, HostKey: c.HostKey,
	}
}

// BackendKey is the name FindBackend, Hold and Locate take for this machine:
// the backend, or the brokered variant of it.
func BackendKey(backend string, broker bool) string {
	if broker && backend == "hyperv" {
		return brokerBackend
	}
	return backend
}

// BackendKey is BackendKey for a Spec.
func (s Spec) BackendKey() string { return BackendKey(s.Backend, s.Broker) }

func stateName(s State) string { return s.String() }

func parseStateName(s string) State {
	switch s {
	case "running":
		return Running
	case "stopped":
		return Stopped
	default:
		return Absent
	}
}
