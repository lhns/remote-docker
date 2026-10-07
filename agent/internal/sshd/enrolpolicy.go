package sshd

import (
	"fmt"

	"github.com/lhns/remote-docker/core/enrol"
)

// check is one account-management operation and every fact authorize decides
// it on (ADR 0053). Pure, so the whole policy is one table test.
type check struct {
	Op     string
	Caller string
	Admin  bool // the caller is named in WORKSPACE_ADMINS

	// Target is the account acted on; empty is a new one, for an unbound
	// token, or a token that is unbound.
	Target      string
	TargetAdmin bool

	// For a removal: the target's keys before and after it, and how many
	// OTHER admins hold a key. The last admin with a key cannot be removed.
	TargetKeys, KeysLeft int
	OtherAdmins          int

	// OperatorDir is an operator directory that enrols the target, for
	// user.rm, or holds the key, for key.rm: this workspace does not write
	// there.
	OperatorDir string

	// LiveTokens is, for token.create by a non-admin, how many unexpired
	// tokens they have minted and not yet seen redeemed or removed.
	LiveTokens int

	// Connected is key.rm of the key this connection authenticated with.
	Connected bool
	Force     bool
}

// denials words a non-admin's refusal, per operation.
var denials = map[string]string{
	enrol.OpTokenCreate: "create a token for another account",
	enrol.OpTokenList:   "list another account's tokens",
	enrol.OpTokenRemove: "remove another account's token",
	enrol.OpUserList:    "list the accounts",
	enrol.OpUserRemove:  "remove an account",
	enrol.OpKeyList:     "list another account's keys",
	enrol.OpKeyAdd:      "add a key to another account",
	enrol.OpKeyRemove:   "remove another account's key",
}

// maxSelfTokens caps the live tokens a non-admin may hold at once, so an
// account cannot fill the state volume with 7-day tokens. Admins and the
// operator's `remote-dockerd token create` are uncapped.
const maxSelfTokens = 10

// authorize decides an operation, nil meaning allowed.
func authorize(c check) *enrol.Error {
	if c.Op == enrol.OpWhoami {
		return nil
	}
	if !c.Admin {
		adminOnly := c.Op == enrol.OpUserList || c.Op == enrol.OpUserRemove
		if adminOnly || c.Target != c.Caller {
			return denied(c)
		}
	}

	// A quota rather than a permission, but it is the same caller-facts
	// decision, so it lives in the one table.
	if c.Op == enrol.OpTokenCreate && !c.Admin && c.LiveTokens >= maxSelfTokens {
		return &enrol.Error{Code: enrol.CodeDenied,
			Msg: fmt.Sprintf("you already have %d unused tokens", c.LiveTokens),
			Fix: "remove one with `remote token rm <id>`, or wait for one to expire"}
	}

	switch c.Op {
	case enrol.OpUserRemove:
		if c.Target == c.Caller {
			return &enrol.Error{Code: enrol.CodeDenied,
				Msg: "an admin cannot remove their own account",
				Fix: "ask another admin"}
		}
		if c.OperatorDir != "" {
			return &enrol.Error{Code: enrol.CodeDenied,
				Msg: fmt.Sprintf("%s's keys are in %s, which this workspace does not write", c.Target, c.OperatorDir),
				Fix: "remove them there, or from the chart's authorizedKeys"}
		}
	case enrol.OpKeyRemove:
		if c.OperatorDir != "" {
			return &enrol.Error{Code: enrol.CodeDenied,
				Msg: fmt.Sprintf("that key is in %s, which this workspace does not write", c.OperatorDir),
				Fix: "remove it there, or from the chart's authorizedKeys"}
		}
	default:
		return nil
	}

	// No -f: an operator would have to edit WORKSPACE_ADMINS to recover.
	if c.TargetAdmin && c.TargetKeys > 0 && c.KeysLeft == 0 && c.OtherAdmins == 0 {
		return &enrol.Error{Code: enrol.CodeDenied,
			Msg: fmt.Sprintf("%s is the last admin with a key", c.Target),
			Fix: "enrol another admin first: name them in WORKSPACE_ADMINS and send them a token"}
	}
	if c.Op == enrol.OpKeyRemove && c.Target == c.Caller && !c.Force {
		switch {
		case c.Connected:
			return &enrol.Error{Code: enrol.CodeForce,
				Msg: "this is the key this machine is connected with, and removing it locks this machine out",
				Fix: "add -f to remove it anyway"}
		case c.KeysLeft == 0:
			return &enrol.Error{Code: enrol.CodeForce,
				Msg: "this is your last key, and removing it locks you out",
				Fix: "add -f to remove it anyway"}
		}
	}
	return nil
}

func denied(c check) *enrol.Error {
	what := denials[c.Op]
	fix := "ask an admin"
	if c.Op == enrol.OpTokenCreate {
		if c.Target == "" {
			what = "create a token for a new account"
			fix = "ask an admin, or the operator: `remote-dockerd token create --unbound`"
		} else {
			fix = "ask an admin, or the operator: `remote-dockerd token create --account " + c.Target + "`"
		}
	}
	return &enrol.Error{Code: enrol.CodeDenied,
		Msg: fmt.Sprintf("only an admin can %s (you are %s)", what, c.Caller),
		Fix: fix}
}
