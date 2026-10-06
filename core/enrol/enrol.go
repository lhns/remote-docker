// Package enrol is the contract for enrolling a key with a single-use token
// (ADR 0051): the login a key-less device uses, the banner that tells a
// token-aware workspace from an older one, the redeem command and its frames,
// and the token and invite codecs. Both binaries speak it, so it is one
// protocol entire, as core/notify and core/cache are.
package enrol

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// LoginPrefix is the SSH login of a redeeming connection: the prefix and
	// the token's id. It is checked before the login is folded into an account
	// name, and no folded name can start with it.
	LoginPrefix = "+token:"

	// Version is the redeem protocol's, carried in Banner.
	Version = 1

	// RedeemCommand is the one command a redeeming connection may run. It
	// reads one RedeemRequest and writes one RedeemReply.
	RedeemCommand = "workspace-redeem"

	// EnrolCommand is account management for an enrolled key (ADR 0053).
	EnrolCommand = "workspace-enrol"
)

// Banner is sent to a `+token:` login only. Its absence on a refused login is
// how the client knows the workspace predates tokens.
var Banner = fmt.Sprintf("remote-docker-enrol %d\n", Version)

// IsBanner reports whether msg is a token-aware workspace's banner.
func IsBanner(msg string) bool { return strings.HasPrefix(msg, "remote-docker-enrol ") }

// RedeemRequest is what the client sends. The key is the one the connection
// authenticated with, never a field here.
type RedeemRequest struct {
	Secret  string `json:"secret"`
	Account string `json:"account,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// RedeemReply is the workspace's one answer: an account, or an Error.
type RedeemReply struct {
	Account string `json:"account,omitempty"`
	Created bool   `json:"created,omitempty"`
	// Pending is a key enrolled for an account the workspace is still
	// creating: it authenticates once that finishes.
	Pending bool   `json:"pending,omitempty"`
	Error   *Error `json:"error,omitempty"`
}

// Error is a refusal, worded for the person at the client.
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Fix  string `json:"fix,omitempty"`
}

func (e *Error) Error() string {
	if e.Fix == "" {
		return e.Msg
	}
	return e.Msg + "\n  fix: " + e.Fix
}

// Error codes.
const (
	CodeRefused = "refused" // unknown, used or expired, never which
	CodeName    = "name"    // the account name is not one this token may take
	CodeStorage = "storage" // the workspace cannot store keys
	CodeBusy    = "busy"    // too many failed attempts lately
	CodeFailed  = "failed"  // anything else; the workspace's log has it
)

// Refused is the one answer to a token that is unknown, used or expired, from
// the handshake or from the redeem: saying which would be an oracle.
var Refused = &Error{
	Code: CodeRefused,
	Msg:  "the workspace refused the token: it is unknown, used or expired",
	Fix:  "ask for a new one",
}

// MaxMessage bounds a request or a reply.
const MaxMessage = 64 << 10

// ReadJSON decodes one bounded message.
func ReadJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, MaxMessage)).Decode(v)
}

// A token is `<id>.<secret>`. The id is public: it is the login, and it names
// the token in listings. The secret is 128 bits and only its hash is stored.
const (
	idBytes     = 5  // 8 base32 characters
	secretBytes = 16 // 22 base64url characters
	IDLength    = 8
)

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewToken returns a fresh id and secret.
func NewToken() (id, secret string, err error) {
	b := make([]byte, idBytes+secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	return idEncoding.EncodeToString(b[:idBytes]), base64.RawURLEncoding.EncodeToString(b[idBytes:]), nil
}

// ValidID reports whether id has a token id's shape, which also makes it safe
// as a file name.
func ValidID(id string) bool {
	if len(id) != IDLength {
		return false
	}
	b, err := idEncoding.DecodeString(id)
	return err == nil && len(b) == idBytes
}

// ParseToken splits a token into its id and secret.
func ParseToken(token string) (id, secret string, err error) {
	id, secret, ok := strings.Cut(token, ".")
	if !ok || !ValidID(id) {
		return "", "", errors.New("not a token")
	}
	if b, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(b) != secretBytes {
		return "", "", errors.New("not a token")
	}
	return id, secret, nil
}

// InvitePrefix starts every invite, and names its version.
const InvitePrefix = "rdt1."

// Invite is everything a device needs to enrol: where the workspace is, the
// host key it must offer, the account the token is bound to, if any, and the
// token. One shell-safe word.
type Invite struct {
	URL     string `json:"u"`
	HostKey string `json:"k"` // SHA256:..., as ssh.FingerprintSHA256 writes it
	Account string `json:"a,omitempty"`
	Token   string `json:"t"`
}

// String is the invite as a person pastes it.
func (i Invite) String() string {
	b, _ := json.Marshal(i)
	return InvitePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// ErrMalformed is any string that is not an invite.
var ErrMalformed = errors.New("this is not an enrolment invite")

// ParseInvite reads what Invite.String wrote.
func ParseInvite(s string) (Invite, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), InvitePrefix)
	if !ok {
		return Invite{}, ErrMalformed
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return Invite{}, ErrMalformed
	}
	var inv Invite
	if err := json.Unmarshal(b, &inv); err != nil {
		return Invite{}, ErrMalformed
	}
	if inv.URL == "" || !strings.HasPrefix(inv.HostKey, "SHA256:") {
		return Invite{}, ErrMalformed
	}
	if _, _, err := ParseToken(inv.Token); err != nil {
		return Invite{}, ErrMalformed
	}
	return inv, nil
}
