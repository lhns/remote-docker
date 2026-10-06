package sshd

// Redeeming an enrolment token (ADR 0051). A device with no enrolled key logs
// in as `+token:<id>` with its own key; the agent accepts that key for a live
// token's id only, the connection gets no account, and the one thing it can
// do is run enrol.RedeemCommand once, which enrols the key it authenticated
// with.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

// redeemerKey is the context key of a token login's redeemer.
type redeemerKey struct{}

// redeemer is a connection that logged in with a token's id.
type redeemer struct {
	id   string
	key  ssh.PublicKey
	used *atomic.Bool // one redemption per connection
}

func redeemerFor(ctx gssh.Context) (redeemer, bool) {
	r, ok := ctx.Value(redeemerKey{}).(redeemer)
	return r, ok
}

// reservedNames are never given to an unbound token's account, and nor are
// the admins' names.
var reservedNames = map[string]bool{
	"root": true, "admin": true, "administrator": true, "nobody": true,
	"operator": true, "docker": true, "workspace": true,
}

// authenticateToken accepts any key for the id of a live token, and nothing
// else: the secret is checked by the redeem, on a connection that can do
// nothing but redeem. Read-only, since nobody is authenticated yet.
func (s *Server) authenticateToken(ctx gssh.Context, id string, key gssh.PublicKey) bool {
	if s.cfg.Tokens == nil || !s.cfg.Tokens.Live(id) {
		// The id only if it is one: a pasted token would put its secret here.
		if !enrol.ValidID(id) {
			id = ""
		}
		s.log().Warn("refused a token login: no such live token", "token", id, "from", ctx.RemoteAddr())
		return false
	}
	ctx.SetValue(contextKey{}, nil)
	ctx.SetValue(redeemerKey{}, redeemer{id: id, key: key, used: new(atomic.Bool)})
	return true
}

// banner marks a token login's handshake, so a client can tell a workspace
// that refused its token from one that predates tokens. Stock ssh, logging in
// as an account, sees nothing.
func banner(ctx gssh.Context) string {
	if strings.HasPrefix(ctx.User(), enrol.LoginPrefix) {
		return enrol.Banner
	}
	return ""
}

// route sends a token login's sessions to the redeem, account management to
// serveEnrol, and every other to handleSession.
func (s *Server) route(session gssh.Session) {
	if r, ok := redeemerFor(session.Context()); ok {
		s.serveRedeem(session, r)
		return
	}
	if account, ok := accountFor(session.Context()); ok && strings.Join(session.Command(), " ") == enrol.EnrolCommand {
		s.serveEnrol(session, account)
		return
	}
	s.handleSession(session)
}

func (s *Server) serveRedeem(session gssh.Session, r redeemer) {
	if strings.Join(session.Command(), " ") != enrol.RedeemCommand || r.used.Swap(true) {
		_, _ = fmt.Fprintf(session.Stderr(), "a token login may only run %s, once\n", enrol.RedeemCommand)
		_ = session.Exit(1)
		return
	}

	var reply enrol.RedeemReply
	var req enrol.RedeemRequest
	if err := enrol.ReadJSON(session, &req); err != nil {
		reply.Error = &enrol.Error{Code: enrol.CodeFailed, Msg: "the workspace could not read the redeem request: " + err.Error()}
	} else {
		reply = s.redeem(r, req, session.RemoteAddr())
	}
	_ = json.NewEncoder(session).Encode(reply)
	_ = session.Exit(0)
}

// redeem enrols the connection's key with the token's secret. A failure on
// the workspace's side puts the token back; a failure on the redeemer's
// spends an attempt from the global limiter.
func (s *Server) redeem(r redeemer, req enrol.RedeemRequest, from net.Addr) (reply enrol.RedeemReply) {
	start := time.Now()
	defer func() {
		outcome := redeemOutcome(reply)
		s.cfg.Metrics.Redemptions.Inc(outcome)
		s.cfg.Metrics.RedemptionSeconds.Since(start, outcome)
	}()
	audit := s.log().With(logx.ComponentKey, "audit", "op", "token.redeem",
		"token", r.id, "key", ssh.FingerprintSHA256(r.key), "from", from)
	refuse := func(e *enrol.Error, why string, args ...any) enrol.RedeemReply {
		audit.Warn("refused: "+why, args...)
		return enrol.RedeemReply{Error: e}
	}

	if !s.limiter.Allow() {
		s.cfg.Metrics.LimiterRejections.Inc()
		return refuse(&enrol.Error{Code: enrol.CodeBusy,
			Msg: "the workspace is refusing tokens for now: too many failed attempts",
			Fix: "try again in a minute"}, "too many failed redemptions")
	}
	if err := s.cfg.Accounts.CheckWritable(); err != nil {
		return refuse(CannotStore(s.cfg.Accounts.EnrolledDir), "cannot store keys", "err", err)
	}
	tok, err := s.cfg.Tokens.Check(r.id, req.Secret)
	if err != nil {
		s.limiter.Fail()
		return refuse(enrol.Refused, "the secret did not match a live token")
	}
	name, create, nameErr := s.redeemName(tok.Account, req.Account)
	if nameErr != nil {
		return refuse(nameErr, "the name", "asked", req.Account, "bound", tok.Account)
	}
	claim, err := s.cfg.Tokens.Consume(r.id, req.Secret)
	if err != nil {
		return refuse(enrol.Refused, "another redemption took the token")
	}

	// A key for an account still being created cannot authenticate yet, and is
	// kept: the account appears when its useradd finishes.
	created, undo, err := s.enrolKey(name, create, r.key, req.Comment)
	pending := errors.Is(err, accounts.ErrProvisioning)
	if pending {
		err = nil
	} else if err == nil && !s.stillAuthorized(name, r.key) {
		undo()
		err = errors.New("the key was written but does not authenticate")
	}
	if err != nil {
		_ = claim.Restore()
		if errors.Is(err, accounts.ErrAccountExists) {
			return refuse(nameTaken(name), "the account was created meanwhile", "account", name)
		}
		return refuse(&enrol.Error{Code: enrol.CodeFailed,
			Msg: "the workspace could not enrol the key; its log says why"},
			"enrolling the key", "account", name, "err", err)
	}
	// The claim is the commit: one withdrawn meanwhile (user rm) takes the key
	// out again.
	if err := claim.Done(); errors.Is(err, os.ErrNotExist) {
		undo()
		return refuse(enrol.Refused, "the token was withdrawn during the redemption", "account", name)
	} else if err != nil {
		audit.Warn("the redeemed token could not be deleted", "err", err)
	}
	audit.Info("redeemed a token", "account", name, "created", created, "bound", tok.Account != "", "pending", pending)
	return enrol.RedeemReply{Account: name, Created: created, Pending: pending}
}

// redeemOutcome labels a reply for the Redemptions counter: failed when the
// workspace could not do its part, refused when the redeemer was turned away.
func redeemOutcome(reply enrol.RedeemReply) string {
	switch {
	case reply.Error == nil && reply.Pending:
		return RedeemPending
	case reply.Error == nil:
		return RedeemOK
	case reply.Error.Code == enrol.CodeFailed || reply.Error.Code == enrol.CodeStorage:
		return RedeemFailed
	}
	return RedeemRefused
}

// redeemName is the account a token enrols into, and whether it must be new.
// A bound token enrols into its account, created or not; an unbound one only
// creates, under a name that has never existed and is not reserved.
func (s *Server) redeemName(bound, asked string) (string, bool, *enrol.Error) {
	if asked != "" {
		name, err := workspace.AccountName(asked)
		if err != nil {
			return "", false, &enrol.Error{Code: enrol.CodeName, Msg: err.Error(), Fix: "choose another name with --user"}
		}
		asked = name
	}
	if bound != "" {
		if asked != "" && asked != bound {
			return "", false, &enrol.Error{Code: enrol.CodeName,
				Msg: fmt.Sprintf("this token is for account %s, not %s", bound, asked),
				Fix: "leave out --user, or use --user " + bound}
		}
		return bound, false, nil
	}
	switch {
	case asked == "":
		return "", false, &enrol.Error{Code: enrol.CodeName,
			Msg: "this token creates a new account, and no name was given", Fix: "name it with --user"}
	case reservedNames[asked] || s.cfg.Admins[asked]:
		return "", false, &enrol.Error{Code: enrol.CodeName,
			Msg: fmt.Sprintf("the name %s is reserved", asked), Fix: "choose another name with --user"}
	}
	known, err := s.cfg.Accounts.Known(asked)
	if err != nil {
		return "", false, &enrol.Error{Code: enrol.CodeFailed, Msg: "the workspace could not read its accounts; its log says why"}
	}
	if known {
		return "", false, nameTaken(asked)
	}
	return asked, true, nil
}

// enrolKey writes the key, and returns how to take it out again. The key is
// written when err is nil or accounts.ErrProvisioning.
func (s *Server) enrolKey(name string, create bool, key ssh.PublicKey, comment string) (created bool, undo func(), err error) {
	if create {
		err := s.cfg.Accounts.CreateAccount(name, key, comment)
		if err != nil && !errors.Is(err, accounts.ErrProvisioning) {
			return false, nil, err
		}
		return true, func() { _ = s.cfg.Accounts.RemoveAccountFile(name) }, err
	}
	known, err := s.cfg.Accounts.Known(name)
	if err != nil {
		return false, nil, err
	}
	added, err := s.cfg.Accounts.AppendKey(name, key, comment)
	if err != nil && !errors.Is(err, accounts.ErrProvisioning) {
		return false, nil, err
	}
	return !known, func() {
		if added {
			_, _ = s.cfg.Accounts.RemoveKey(name, ssh.FingerprintSHA256(key))
		}
	}, err
}

func nameTaken(name string) *enrol.Error {
	return &enrol.Error{Code: enrol.CodeName,
		Msg: fmt.Sprintf("the account %s already exists", name),
		Fix: "choose another name with --user, or ask for a token for " + name}
}

// CannotStore is the refusal for a workspace whose enrolled keys directory
// cannot be written, from the redeem and from `remote-dockerd token create`.
func CannotStore(dir string) *enrol.Error {
	return &enrol.Error{Code: enrol.CodeStorage,
		Msg: fmt.Sprintf("this workspace cannot store keys: %s is read-only", dir),
		Fix: "point WORKSPACE_ENROLLED_KEYS_DIR at a writable directory"}
}
