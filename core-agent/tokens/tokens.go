// Package tokens stores single-use enrolment tokens (ADR 0051): one file per
// token holding the hash of its secret, never the secret. A redemption claims
// its file by renaming it, which has one winner on NFS and CephFS as well as
// locally, so two agents sharing the directory cannot both redeem one token.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lhns/remote-docker/core/enrol"
)

const (
	// DefaultTTL and MaxTTL bound a token's life.
	DefaultTTL = 24 * time.Hour
	MaxTTL     = 7 * 24 * time.Hour

	// claimStale is how long a claim may sit before Sweep deletes it: a
	// redemption takes milliseconds, so an older one is an agent that died.
	claimStale = 10 * time.Minute

	suffix      = ".json"
	claimPrefix = ".redeeming-"
)

// ErrRefused is every reason a token does not redeem: unknown, used, expired
// or the wrong secret. Callers must not tell them apart to the client.
var ErrRefused = errors.New("the token is unknown, used or expired")

// Token is a stored token, without its secret.
type Token struct {
	ID      string    `json:"-"`
	Hash    string    `json:"hash"`
	Account string    `json:"account,omitempty"` // empty: unbound
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Creator string    `json:"creator,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// Store is a directory of tokens.
type Store struct {
	Dir string

	// Now is the clock, for tests. Nil is time.Now.
	Now func() time.Time
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Mint creates a token for account, or an unbound one for "", and returns the
// token as the redeemer must present it. A ttl of 0 is DefaultTTL.
func (s *Store) Mint(account, creator, note string, ttl time.Duration) (string, Token, error) {
	switch {
	case ttl == 0:
		ttl = DefaultTTL
	case ttl < 0 || ttl > MaxTTL:
		return "", Token{}, fmt.Errorf("a token lives at most %s, not %s", MaxTTL, ttl)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", Token{}, err
	}

	for {
		id, secret, err := enrol.NewToken()
		if err != nil {
			return "", Token{}, err
		}
		now := s.now().UTC().Truncate(time.Second)
		t := Token{ID: id, Hash: hash(secret), Account: account, Created: now,
			Expires: now.Add(ttl), Creator: creator, Note: note}
		err = s.create(t)
		if errors.Is(err, os.ErrExist) {
			continue // an id collision, 1 in 2^40
		}
		if err != nil {
			return "", Token{}, err
		}
		return id + "." + secret, t, nil
	}
}

// create writes a token's file whole, through a temporary and a link, which
// refuses an id already taken.
func (s *Store) create(t Token) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".mint-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Link(tmp.Name(), s.path(t.ID))
}

// List is every token not yet redeemed, expired ones included, oldest first.
func (s *Store) List() ([]Token, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Token
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), suffix)
		if !ok || !enrol.ValidID(id) {
			continue
		}
		t, err := read(s.path(id))
		if err != nil {
			continue // redeemed or revoked since the listing
		}
		t.ID = id
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Token) int { return a.Created.Compare(b.Created) })
	return out, nil
}

// Revoke deletes a token. An id that names nothing is an error.
func (s *Store) Revoke(id string) error {
	if !enrol.ValidID(id) {
		return fmt.Errorf("%q is not a token id", id)
	}
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return fmt.Errorf("no token %s", id)
	}
	return err
}

// Live reports whether id names a token that has not expired. It reads and
// never writes, since it answers an unauthenticated login.
func (s *Store) Live(id string) bool {
	if !enrol.ValidID(id) {
		return false
	}
	t, err := read(s.path(id))
	return err == nil && s.now().Before(t.Expires)
}

// Check returns the token id names if secret is its secret and it has not
// expired, and ErrRefused otherwise. It changes nothing.
func (s *Store) Check(id, secret string) (Token, error) {
	if !enrol.ValidID(id) {
		return Token{}, ErrRefused
	}
	t, err := read(s.path(id))
	if err != nil {
		return Token{}, ErrRefused
	}
	if !s.matches(t, secret) {
		return Token{}, ErrRefused
	}
	t.ID = id
	return t, nil
}

func (s *Store) matches(t Token, secret string) bool {
	want, err := hex.DecodeString(t.Hash)
	got := sha256.Sum256([]byte(secret))
	return err == nil && subtle.ConstantTimeCompare(want, got[:]) == 1 && s.now().Before(t.Expires)
}

// Claim is a token taken out of the store by Consume, until Done deletes it
// or Restore puts it back.
type Claim struct {
	Token
	from, at string
}

// Consume takes the token out of the store, so no other redemption can have
// it. Of concurrent calls, one wins and the rest get ErrRefused.
func (s *Store) Consume(id, secret string) (*Claim, error) {
	if !enrol.ValidID(id) {
		return nil, ErrRefused
	}
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	c := &Claim{
		from: s.path(id),
		at: filepath.Join(s.Dir, fmt.Sprintf("%s%s-%d-%s", claimPrefix, id,
			s.now().UnixNano(), hex.EncodeToString(nonce))),
	}
	if err := os.Rename(c.from, c.at); err != nil {
		return nil, ErrRefused
	}
	// Checked again on what was claimed: the file may have changed since a
	// Check, and the claim is what is redeemed.
	t, err := read(c.at)
	if err != nil || !s.matches(t, secret) {
		_ = c.Restore()
		return nil, ErrRefused
	}
	t.ID = id
	c.Token = t
	return c, nil
}

// Restore puts a claimed token back, for a redemption that failed on the
// workspace's side rather than the redeemer's.
func (c *Claim) Restore() error { return os.Rename(c.at, c.from) }

// Done deletes a claimed token: it has been redeemed.
func (c *Claim) Done() error { return os.Remove(c.at) }

// Sweep deletes expired tokens, and claims an agent died holding. It returns
// how many it deleted.
func (s *Store) Sweep() (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(s.Dir, name)
		if rest, ok := strings.CutPrefix(name, claimPrefix); ok {
			if at, ok := claimedAt(rest); ok && s.now().Sub(at) > claimStale && os.Remove(path) == nil {
				n++
			}
			continue
		}
		if id, ok := strings.CutSuffix(name, suffix); ok && enrol.ValidID(id) {
			if t, err := read(path); err == nil && !s.now().Before(t.Expires) && os.Remove(path) == nil {
				n++
			}
		}
	}
	return n, nil
}

// claimedAt reads the time out of a claim's name, <id>-<unixnano>-<rand>.
func claimedAt(rest string) (time.Time, bool) {
	parts := strings.Split(rest, "-")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	ns, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, ns), true
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, id+suffix) }

func read(path string) (Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Token{}, err
	}
	var t Token
	if err := json.Unmarshal(data, &t); err != nil {
		return Token{}, err
	}
	return t, nil
}

func hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
