package tokens

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lhns/remote-docker/core/enrol"
)

type clock struct{ t atomic.Pointer[time.Time] }

func newClock() *clock {
	c := &clock{}
	c.set(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	return c
}
func (c *clock) now() time.Time          { return *c.t.Load() }
func (c *clock) set(t time.Time)         { c.t.Store(&t) }
func (c *clock) advance(d time.Duration) { c.set(c.now().Add(d)) }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := newClock()
	return &Store{Dir: filepath.Join(t.TempDir(), "tokens"), Now: c.now}, c
}

func mint(t *testing.T, s *Store, account string) (id, secret string) {
	t.Helper()
	token, _, err := s.Mint(account, "operator", "", 0)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	id, secret, err = enrol.ParseToken(token)
	if err != nil {
		t.Fatalf("Mint returned %q: %v", token, err)
	}
	return id, secret
}

func TestOnlyAHashOfTheSecretIsStored(t *testing.T) {
	s, _ := newStore(t)
	id, secret := mint(t, s, "alice")

	data, err := os.ReadFile(s.path(id))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Errorf("the token file holds the secret: %s", data)
	}
	info, err := os.Stat(s.path(id))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); os.PathSeparator == '/' && mode != 0o600 {
		t.Errorf("the token file is %v, want 0600", mode)
	}
}

func TestATokenRedeemsOnce(t *testing.T) {
	s, _ := newStore(t)
	id, secret := mint(t, s, "alice")

	if !s.Live(id) {
		t.Fatal("a fresh token is not live")
	}
	tok, err := s.Check(id, secret)
	if err != nil || tok.Account != "alice" {
		t.Fatalf("Check = %+v, %v", tok, err)
	}
	claim, err := s.Consume(id, secret)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if s.Live(id) {
		t.Error("a claimed token is still live")
	}
	if err := claim.Done(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Consume(id, secret); !errors.Is(err, ErrRefused) {
		t.Errorf("a redeemed token was consumed again: %v", err)
	}
	if entries, _ := os.ReadDir(s.Dir); len(entries) != 0 {
		t.Errorf("a redeemed token left files: %v", entries)
	}
}

func TestARestoredTokenRedeemsAgain(t *testing.T) {
	s, _ := newStore(t)
	id, secret := mint(t, s, "")
	claim, err := s.Consume(id, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := claim.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Check(id, secret); err != nil {
		t.Errorf("a restored token does not check: %v", err)
	}
}

func TestEveryRefusalIsTheSame(t *testing.T) {
	s, c := newStore(t)
	id, secret := mint(t, s, "alice")
	other, _ := mint(t, s, "alice")

	for name, args := range map[string][2]string{
		"wrong secret":   {id, strings.Repeat("A", len(secret))},
		"another's id":   {other, secret},
		"unknown id":     {"aaaaaaaa", secret},
		"not an id":      {"../" + id, secret},
		"empty":          {"", ""},
		"secret for all": {id, ""},
	} {
		if _, err := s.Check(args[0], args[1]); err != ErrRefused {
			t.Errorf("%s: Check = %v, want ErrRefused", name, err)
		}
		if _, err := s.Consume(args[0], args[1]); err != ErrRefused {
			t.Errorf("%s: Consume = %v, want ErrRefused", name, err)
		}
	}
	if !s.Live(id) {
		t.Fatal("a refused Consume took the token")
	}

	c.advance(DefaultTTL)
	if s.Live(id) {
		t.Error("an expired token is live")
	}
	if _, err := s.Check(id, secret); err != ErrRefused {
		t.Errorf("an expired token checks: %v", err)
	}
	if _, err := s.Consume(id, secret); err != ErrRefused {
		t.Errorf("an expired token was consumed: %v", err)
	}
}

// rename(2) of a missing file fails, which is the whole lock. Windows renames
// one file to several names at once, and the agent runs on Linux only.
func TestConcurrentRedemptionsHaveOneWinner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename is not one-winner on Windows")
	}
	s, _ := newStore(t)
	id, secret := mint(t, s, "alice")

	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := s.Consume(id, secret); err == nil {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	if n := wins.Load(); n != 1 {
		t.Errorf("%d redemptions won one token", n)
	}
}

func TestLiveWritesNothing(t *testing.T) {
	s, _ := newStore(t)
	id, _ := mint(t, s, "alice")
	before, _ := os.ReadFile(s.path(id))
	for range 3 {
		s.Live(id)
		s.Live("aaaaaaaa")
	}
	after, _ := os.ReadFile(s.path(id))
	entries, _ := os.ReadDir(s.Dir)
	if string(before) != string(after) || len(entries) != 1 {
		t.Errorf("Live changed the store: %d entries", len(entries))
	}
}

func TestATokenLivesAtMostSevenDays(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Mint("alice", "", "", MaxTTL+time.Second); err == nil {
		t.Error("a token longer than the maximum was minted")
	}
	if _, _, err := s.Mint("alice", "", "", -time.Second); err == nil {
		t.Error("a token with a negative life was minted")
	}
	_, tok, err := s.Mint("alice", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := tok.Expires.Sub(tok.Created); got != DefaultTTL {
		t.Errorf("the default life is %s, want %s", got, DefaultTTL)
	}
}

func TestSweepTakesExpiredTokensAndDeadClaims(t *testing.T) {
	s, c := newStore(t)
	short, _, err := s.Mint("alice", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	shortID, _, _ := enrol.ParseToken(short)
	keepID, _ := mint(t, s, "bob")
	claimID, claimSecret := mint(t, s, "carol")
	if _, err := s.Consume(claimID, claimSecret); err != nil {
		t.Fatal(err)
	}

	if n, err := s.Sweep(); err != nil || n != 0 {
		t.Fatalf("a sweep of nothing stale took %d: %v", n, err)
	}
	c.advance(2 * time.Hour)
	if n, err := s.Sweep(); err != nil || n != 2 {
		t.Fatalf("Sweep = %d, %v; want the expired token and the dead claim", n, err)
	}
	if s.Live(shortID) || !s.Live(keepID) {
		t.Error("the sweep took the wrong token")
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].ID != keepID {
		t.Errorf("List = %+v, %v", list, err)
	}
}

func TestRevoke(t *testing.T) {
	s, _ := newStore(t)
	id, _ := mint(t, s, "alice")
	if err := s.Revoke(id); err != nil {
		t.Fatal(err)
	}
	if s.Live(id) {
		t.Error("a revoked token is live")
	}
	if err := s.Revoke(id); err == nil {
		t.Error("revoking nothing succeeded")
	}
	if err := s.Revoke("../x"); err == nil {
		t.Error("a path was accepted as an id")
	}
}

func TestTheLimiterSpendsFailuresAndRefills(t *testing.T) {
	c := newClock()
	l := &Limiter{Burst: 3, Every: 6 * time.Second, Now: c.now}
	for range 3 {
		if !l.Allow() {
			t.Fatal("refused inside the burst")
		}
		l.Fail()
	}
	if l.Allow() {
		t.Fatal("allowed past the burst")
	}
	c.advance(6 * time.Second)
	if !l.Allow() {
		t.Fatal("one interval refunded nothing")
	}
	l.Fail()
	if l.Allow() {
		t.Fatal("one interval refunded more than one")
	}
}
