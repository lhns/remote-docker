package enrol

import "time"

// Account management over EnrolCommand (ADR 0053): one Request in, one Reply
// out, on a connection that authenticated as an account. The caller is that
// account, never a field here.

// Operations.
const (
	OpWhoami      = "whoami"
	OpTokenCreate = "token.create"
	OpTokenList   = "token.ls"
	OpTokenRemove = "token.rm"
	OpUserList    = "user.ls"
	OpUserRemove  = "user.rm"
	OpKeyList     = "key.ls"
	OpKeyAdd      = "key.add"
	OpKeyRemove   = "key.rm"
)

// More error codes, for account management.
const (
	CodeDenied  = "denied"  // the caller may not do this
	CodeForce   = "force"   // allowed with -f
	CodeUnknown = "unknown" // no such account, token, key or operation
)

// Request is one operation. Account is the account it acts on, empty for the
// caller's own; Unbound asks token.create for a token that creates a new one.
type Request struct {
	Op          string        `json:"op"`
	Account     string        `json:"account,omitempty"`
	Unbound     bool          `json:"unbound,omitempty"`
	Expires     time.Duration `json:"expires,omitempty"`
	Note        string        `json:"note,omitempty"`
	ID          string        `json:"id,omitempty"`  // token.rm
	Key         string        `json:"key,omitempty"` // key.add: an authorized_keys line
	Fingerprint string        `json:"fingerprint,omitempty"`
	Force       bool          `json:"force,omitempty"`
	Purge       bool          `json:"purge,omitempty"` // user.rm: the account's storage, home and unix user too
}

// Reply is the one answer. Error is set on a refusal; Notices are things the
// person should know about an operation that succeeded.
type Reply struct {
	Error   *Error   `json:"error,omitempty"`
	Notices []*Error `json:"notices,omitempty"`

	Whoami *Whoami     `json:"whoami,omitempty"`
	Token  *TokenInfo  `json:"token,omitempty"` // token.create, with Token set
	Tokens []TokenInfo `json:"tokens,omitempty"`
	Users  []User      `json:"users,omitempty"`
	Keys   []Key       `json:"keys,omitempty"`
}

// Whoami is the caller as the workspace sees it.
type Whoami struct {
	Account string `json:"account"`
	Admin   bool   `json:"admin,omitempty"`
	Key     string `json:"key"` // the fingerprint this connection used
}

// TokenInfo is a token without its secret, except from token.create.
type TokenInfo struct {
	ID      string    `json:"id"`
	Token   string    `json:"token,omitempty"`
	Account string    `json:"account,omitempty"` // empty: unbound
	Expires time.Time `json:"expires"`
	Creator string    `json:"creator,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// User is one account.
type User struct {
	Name    string      `json:"name"`
	UID     int         `json:"uid,omitempty"` // 0: not enrolled yet
	Sources []KeySource `json:"sources,omitempty"`
	Admin   bool        `json:"admin,omitempty"`
	State   string      `json:"state"`
}

// User states.
const (
	StateConnected   = "connected"
	StateEnrolled    = "enrolled"
	StateRevoked     = "revoked"      // known, with no key
	StateNotEnrolled = "not enrolled" // an admin name nobody holds
)

// KeySource is how many keys one directory holds for an account.
type KeySource struct {
	Dir      string `json:"dir"`
	Operator bool   `json:"operator,omitempty"`
	Keys     int    `json:"keys"`
}

// Key is one enrolled key.
type Key struct {
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment,omitempty"`
	Dir         string `json:"dir"`
	Operator    bool   `json:"operator,omitempty"`
	Current     bool   `json:"current,omitempty"` // the key this connection used
}
