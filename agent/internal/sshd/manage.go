package sshd

// Account management for an enrolled key (ADR 0053): enrol.EnrolCommand reads
// one enrol.Request and writes one enrol.Reply. The caller is the account the
// connection authenticated as, and authorize decides what it may do.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core-agent/tokens"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/logx"
	"github.com/lhns/remote-docker/core/workspace"
)

func (s *Server) serveEnrol(session gssh.Session, caller sessionAccount) {
	// Removing the key this connection uses closes it; the reply goes first.
	defer s.conns.hold(session.Context())()

	var reply enrol.Reply
	var req enrol.Request
	if err := enrol.ReadJSON(session, &req); err != nil {
		reply.Error = &enrol.Error{Code: enrol.CodeFailed, Msg: "the workspace could not read the request: " + err.Error()}
	} else {
		reply = s.manage(session.Context(), caller, req, session.RemoteAddr())
	}
	_ = json.NewEncoder(session).Encode(reply)
	_ = session.Exit(0)
}

// manage runs one operation as caller, and audits a refusal.
func (s *Server) manage(ctx context.Context, caller sessionAccount, req enrol.Request, from net.Addr) enrol.Reply {
	audit := s.log().With(logx.ComponentKey, "audit", "op", req.Op, "by", caller.name, "from", from)
	reply := s.operate(ctx, caller, req, audit)
	if reply.Error != nil {
		audit.Warn("refused: "+reply.Error.Msg, "account", req.Account)
	}
	return reply
}

func (s *Server) operate(ctx context.Context, caller sessionAccount, req enrol.Request, audit *slog.Logger) enrol.Reply {
	target := caller.name
	if req.Account != "" {
		name, err := workspace.AccountName(req.Account)
		if err != nil {
			return failed(&enrol.Error{Code: enrol.CodeName, Msg: err.Error()})
		}
		target = name
	}

	switch req.Op {
	case enrol.OpWhoami:
		return enrol.Reply{Whoami: &enrol.Whoami{Account: caller.name, Admin: s.cfg.Admins[caller.name], Key: caller.fingerprint}}
	case enrol.OpTokenCreate:
		if req.Unbound {
			if req.Account != "" {
				return failed(&enrol.Error{Code: enrol.CodeName, Msg: "a token is either for an account or unbound, not both"})
			}
			target = ""
		}
		return s.tokenCreate(caller, target, req, audit)
	case enrol.OpTokenList:
		return s.tokenList(caller, target, req.Account == "")
	case enrol.OpTokenRemove:
		return s.tokenRemove(caller, req.ID, audit)
	case enrol.OpUserList:
		if e := authorize(s.checkFor(req.Op, caller, "")); e != nil {
			return failed(e)
		}
		return enrol.Reply{Users: s.users()}
	case enrol.OpUserRemove:
		if req.Account == "" {
			return failed(&enrol.Error{Code: enrol.CodeName, Msg: "name the account to remove"})
		}
		return s.userRemove(ctx, caller, target, req.Force, audit)
	case enrol.OpKeyList:
		return s.keyList(caller, target)
	case enrol.OpKeyAdd:
		return s.keyAdd(caller, target, req.Key, audit)
	case enrol.OpKeyRemove:
		return s.keyRemove(caller, target, req.Fingerprint, req.Force, audit)
	}
	return failed(&enrol.Error{Code: enrol.CodeUnknown,
		Msg: fmt.Sprintf("this workspace's agent does not know the operation %q", req.Op),
		Fix: "ask its operator to upgrade it"})
}

func failed(e *enrol.Error) enrol.Reply { return enrol.Reply{Error: e} }

// checkFor gathers what authorize decides op on.
func (s *Server) checkFor(op string, caller sessionAccount, target string) check {
	c := check{Op: op, Caller: caller.name, Admin: s.cfg.Admins[caller.name],
		Target: target, TargetAdmin: s.cfg.Admins[target]}
	if a, ok := s.cfg.Accounts.Lookup(target); ok {
		c.TargetKeys = len(a.Keys)
	}
	for name := range s.cfg.Admins {
		if a, ok := s.cfg.Accounts.Lookup(name); ok && name != target && len(a.Keys) > 0 {
			c.OtherAdmins++
		}
	}
	return c
}

func (s *Server) tokenCreate(caller sessionAccount, target string, req enrol.Request, audit *slog.Logger) enrol.Reply {
	if e := authorize(s.checkFor(req.Op, caller, target)); e != nil {
		return failed(e)
	}
	if s.cfg.Tokens == nil || s.cfg.Accounts.CheckWritable() != nil {
		return failed(CannotStore(s.cfg.Accounts.EnrolledDir))
	}
	token, t, err := s.cfg.Tokens.Mint(target, caller.name, req.Note, req.Expires)
	if err != nil {
		return failed(&enrol.Error{Code: enrol.CodeFailed, Msg: err.Error(), Fix: "pass --expires 168h or less"})
	}
	audit.Info("minted a token", "token", t.ID, "account", target)
	info := tokenInfo(t)
	info.Token = token
	return enrol.Reply{Token: &info}
}

// tokenList is the caller's own tokens, or every token for an admin who names
// no account.
func (s *Server) tokenList(caller sessionAccount, target string, all bool) enrol.Reply {
	if e := authorize(s.checkFor(enrol.OpTokenList, caller, target)); e != nil {
		return failed(e)
	}
	all = all && s.cfg.Admins[caller.name]
	reply := enrol.Reply{Tokens: []enrol.TokenInfo{}}
	if s.cfg.Tokens == nil {
		return reply
	}
	list, err := s.cfg.Tokens.List()
	if err != nil {
		return failed(&enrol.Error{Code: enrol.CodeFailed, Msg: "the workspace could not read its tokens; its log says why"})
	}
	for _, t := range list {
		if all || t.Account == target {
			reply.Tokens = append(reply.Tokens, tokenInfo(t))
		}
	}
	return reply
}

func (s *Server) tokenRemove(caller sessionAccount, id string, audit *slog.Logger) enrol.Reply {
	if s.cfg.Tokens == nil {
		return failed(noToken(id))
	}
	list, err := s.cfg.Tokens.List()
	if err != nil {
		return failed(&enrol.Error{Code: enrol.CodeFailed, Msg: "the workspace could not read its tokens; its log says why"})
	}
	i := slices.IndexFunc(list, func(t tokens.Token) bool { return t.ID == id })
	if i < 0 {
		return failed(noToken(id))
	}
	if e := authorize(s.checkFor(enrol.OpTokenRemove, caller, list[i].Account)); e != nil {
		return failed(e)
	}
	if err := s.cfg.Tokens.Revoke(id); err != nil {
		return failed(noToken(id))
	}
	audit.Info("removed a token", "token", id, "account", list[i].Account)
	return enrol.Reply{}
}

func tokenInfo(t tokens.Token) enrol.TokenInfo {
	return enrol.TokenInfo{ID: t.ID, Account: t.Account, Expires: t.Expires, Creator: t.Creator, Note: t.Note}
}

func noToken(id string) *enrol.Error {
	return &enrol.Error{Code: enrol.CodeUnknown, Msg: fmt.Sprintf("no token %s", id),
		Fix: "`remote token ls` lists them"}
}

func noAccount(name string) *enrol.Error {
	return &enrol.Error{Code: enrol.CodeUnknown, Msg: fmt.Sprintf("no account %s", name),
		Fix: "`remote user ls` lists them"}
}

// users is every account, and every admin name nobody is enrolled under.
func (s *Server) users() []enrol.User {
	var out []enrol.User
	seen := map[string]bool{}
	for _, a := range s.cfg.Accounts.List() {
		seen[a.Name] = true
		u := enrol.User{Name: a.Name, UID: a.UID, Admin: s.cfg.Admins[a.Name], State: enrol.StateEnrolled}
		for _, src := range a.Sources {
			u.Sources = append(u.Sources, enrol.KeySource{Dir: src.Dir, Operator: src.Dir != s.cfg.Accounts.EnrolledDir, Keys: len(src.Keys)})
		}
		switch {
		case len(a.Keys) == 0:
			u.State = enrol.StateRevoked
		case s.conns.connected(a.Name):
			u.State = enrol.StateConnected
		}
		out = append(out, u)
	}
	for name := range s.cfg.Admins {
		if !seen[name] {
			out = append(out, enrol.User{Name: name, Admin: true, State: enrol.StateNotEnrolled})
		}
	}
	slices.SortFunc(out, func(a, b enrol.User) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// userRemove revokes an account and removes its daemon container, keeping
// its storage, home, unix user, uid and ports, so it can come back.
func (s *Server) userRemove(ctx context.Context, caller sessionAccount, target string, force bool, audit *slog.Logger) enrol.Reply {
	c := s.checkFor(enrol.OpUserRemove, caller, target)
	a, ok := s.cfg.Accounts.Lookup(target)
	if ok {
		for _, src := range a.Sources {
			if src.Dir != s.cfg.Accounts.EnrolledDir {
				c.OperatorDir = src.Dir
			}
		}
	}
	if e := authorize(c); e != nil {
		return failed(e) // before saying whether the account exists
	}
	if !ok {
		return failed(noAccount(target))
	}
	if !force {
		fix := "add -f to remove " + target + " anyway"
		switch n := s.cfg.Daemons.Running(ctx, target); {
		case n < 0:
			return failed(&enrol.Error{Code: enrol.CodeForce,
				Msg: fmt.Sprintf("cannot tell whether %s's daemon is running containers", target), Fix: fix})
		case n > 0:
			return failed(&enrol.Error{Code: enrol.CodeForce,
				Msg: fmt.Sprintf("%s's daemon is running %d container(s), and removing %s stops them", target, n, target), Fix: fix})
		}
	}
	if s.cfg.Accounts.CheckWritable() != nil {
		return failed(CannotStore(s.cfg.Accounts.EnrolledDir))
	}
	if err := s.cfg.Accounts.RemoveAccountFile(target); err != nil {
		return failed(writeError(err, audit))
	}

	var reply enrol.Reply
	// A token bound to the account would bring it back.
	if s.cfg.Tokens != nil {
		if list, err := s.cfg.Tokens.List(); err == nil {
			for _, t := range list {
				if t.Account == target && s.cfg.Tokens.Revoke(t.ID) == nil {
					audit.Info("removed a token of a removed account", "token", t.ID, "account", target)
				}
			}
		}
	}
	if err := s.cfg.Daemons.Reset(ctx, target, false); err != nil {
		audit.Warn("could not remove a removed account's daemon", "account", target, "err", err)
		reply.Notices = append(reply.Notices, &enrol.Error{
			Msg: fmt.Sprintf("%s's keys are removed, but their daemon is not: %v", target, err),
			Fix: "`remote-dockerd daemons reset " + target + " -f` inside the workspace"})
	}
	if s.cfg.Daemons.Mode() == workspace.ModeShared {
		reply.Notices = append(reply.Notices, &enrol.Error{
			Msg: fmt.Sprintf("this workspace shares one daemon, so %s's containers were not touched", target)})
	}
	if s.cfg.Admins[target] {
		reply.Notices = append(reply.Notices, &enrol.Error{
			Msg: fmt.Sprintf("%s is still named in WORKSPACE_ADMINS, so whoever is enrolled as %s next is an admin", target, target),
			Fix: "remove " + target + " from WORKSPACE_ADMINS"})
	}
	audit.Info("removed an account", "account", target, "force", force)
	return reply
}

func (s *Server) keyList(caller sessionAccount, target string) enrol.Reply {
	if e := authorize(s.checkFor(enrol.OpKeyList, caller, target)); e != nil {
		return failed(e)
	}
	a, ok := s.cfg.Accounts.Lookup(target)
	if !ok {
		return failed(noAccount(target))
	}
	reply := enrol.Reply{Keys: []enrol.Key{}}
	for _, src := range a.Sources {
		for i, k := range src.Keys {
			fp := ssh.FingerprintSHA256(k)
			reply.Keys = append(reply.Keys, enrol.Key{
				Fingerprint: fp, Comment: src.Comments[i], Dir: src.Dir,
				Operator: src.Dir != s.cfg.Accounts.EnrolledDir,
				Current:  target == caller.name && fp == caller.fingerprint,
			})
		}
	}
	return reply
}

func (s *Server) keyAdd(caller sessionAccount, target, line string, audit *slog.Logger) enrol.Reply {
	key, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return failed(&enrol.Error{Code: enrol.CodeFailed, Msg: "that is not a public key",
			Fix: "pass the .pub file, or one authorized_keys line"})
	}
	if e := authorize(s.checkFor(enrol.OpKeyAdd, caller, target)); e != nil {
		return failed(e)
	}
	if _, ok := s.cfg.Accounts.Lookup(target); !ok {
		return failed(&enrol.Error{Code: enrol.CodeUnknown, Msg: fmt.Sprintf("no account %s", target),
			Fix: "send them a token instead: `remote token create --account " + target + "`"})
	}
	if s.cfg.Accounts.CheckWritable() != nil {
		return failed(CannotStore(s.cfg.Accounts.EnrolledDir))
	}
	added, err := s.cfg.Accounts.AppendKey(target, key, comment)
	if err != nil {
		return failed(writeError(err, audit))
	}
	var reply enrol.Reply
	if !added {
		reply.Notices = []*enrol.Error{{Msg: "that key was already enrolled for " + target}}
	}
	audit.Info("added a key", "account", target, "key", ssh.FingerprintSHA256(key), "dir", s.cfg.Accounts.EnrolledDir)
	return reply
}

func (s *Server) keyRemove(caller sessionAccount, target, fp string, force bool, audit *slog.Logger) enrol.Reply {
	c := s.checkFor(enrol.OpKeyRemove, caller, target)
	c.KeysLeft = c.TargetKeys
	if e := authorize(c); e != nil {
		return failed(e) // before saying whether the key exists
	}
	a, ok := s.cfg.Accounts.Lookup(target)
	if !ok || !slices.ContainsFunc(a.Keys, func(k ssh.PublicKey) bool { return ssh.FingerprintSHA256(k) == fp }) {
		return failed(&enrol.Error{Code: enrol.CodeUnknown, Msg: fmt.Sprintf("%s has no key %s", target, fp),
			Fix: "`remote key ls` lists them"})
	}
	c.KeysLeft = c.TargetKeys - 1
	c.Connected = target == caller.name && fp == caller.fingerprint
	c.Force = force
	if e := authorize(c); e != nil {
		return failed(e)
	}
	if s.cfg.Accounts.CheckWritable() != nil {
		return failed(CannotStore(s.cfg.Accounts.EnrolledDir))
	}
	removed, err := s.cfg.Accounts.RemoveKey(target, fp)
	if err != nil {
		return failed(writeError(err, audit))
	}
	if !removed {
		return failed(&enrol.Error{Code: enrol.CodeUnknown, Msg: fmt.Sprintf("%s has no key %s in %s", target, fp, s.cfg.Accounts.EnrolledDir)})
	}
	audit.Info("removed a key", "account", target, "key", fp, "dir", s.cfg.Accounts.EnrolledDir)
	return enrol.Reply{}
}

// writeError words a failed write of the enrolled directory.
func writeError(err error, audit *slog.Logger) *enrol.Error {
	var op *accounts.OperatorKeyError
	if errors.As(err, &op) {
		return &enrol.Error{Code: enrol.CodeDenied, Msg: op.Error(),
			Fix: "remove it there, or from the chart's authorizedKeys"}
	}
	audit.Warn("writing the enrolled keys failed", "err", err)
	return &enrol.Error{Code: enrol.CodeFailed, Msg: "the workspace could not write its keys; its log says why"}
}
