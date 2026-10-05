package sshd

import (
	"strings"
	"testing"

	"github.com/lhns/remote-docker/core/enrol"
)

// The whole of who may manage what (ADR 0053). alice is an admin, bob is not.
func TestAuthorize(t *testing.T) {
	admin := func(op, target string) check {
		return check{Op: op, Caller: "alice", Admin: true, Target: target, TargetKeys: 1, KeysLeft: 1}
	}
	user := func(op, target string) check {
		return check{Op: op, Caller: "bob", Target: target, TargetKeys: 1, KeysLeft: 1}
	}
	with := func(c check, f func(*check)) check { f(&c); return c }

	for _, tc := range []struct {
		name string
		c    check
		code string // "" is allowed
		msg  string
	}{
		{"anybody asks who they are", user(enrol.OpWhoami, "bob"), "", ""},

		// A non-admin: their own account only, and never the account list.
		{"user: a token for themselves", user(enrol.OpTokenCreate, "bob"), "", ""},
		{"user: a token for another account", user(enrol.OpTokenCreate, "carol"), enrol.CodeDenied,
			"only an admin can create a token for another account (you are bob)"},
		{"user: an unbound token", user(enrol.OpTokenCreate, ""), enrol.CodeDenied,
			"only an admin can create a token for a new account (you are bob)"},
		{"user: their own tokens", user(enrol.OpTokenList, "bob"), "", ""},
		{"user: another's tokens", user(enrol.OpTokenList, "alice"), enrol.CodeDenied, "another account's tokens"},
		{"user: removes their own token", user(enrol.OpTokenRemove, "bob"), "", ""},
		{"user: removes an unbound token", user(enrol.OpTokenRemove, ""), enrol.CodeDenied, "another account's token"},
		{"user: lists accounts", user(enrol.OpUserList, ""), enrol.CodeDenied, "list the accounts"},
		{"user: removes their own account", user(enrol.OpUserRemove, "bob"), enrol.CodeDenied, "remove an account"},
		{"user: removes another", user(enrol.OpUserRemove, "carol"), enrol.CodeDenied, "remove an account"},
		{"user: their own keys", user(enrol.OpKeyList, "bob"), "", ""},
		{"user: adds a key", user(enrol.OpKeyAdd, "bob"), "", ""},
		{"user: adds a key to another", user(enrol.OpKeyAdd, "alice"), enrol.CodeDenied, "add a key to another account"},
		{"user: removes another's key", user(enrol.OpKeyRemove, "alice"), enrol.CodeDenied, "remove another account's key"},
		{"user: removes a spare key of their own", with(user(enrol.OpKeyRemove, "bob"), func(c *check) { c.TargetKeys, c.KeysLeft = 2, 1 }), "", ""},

		// Your own keys: the connected one or the last one needs -f.
		{"own connected key", with(user(enrol.OpKeyRemove, "bob"), func(c *check) { c.TargetKeys, c.KeysLeft, c.Connected = 2, 1, true }),
			enrol.CodeForce, "connected with"},
		{"own connected key, forced", with(user(enrol.OpKeyRemove, "bob"), func(c *check) { c.TargetKeys, c.KeysLeft, c.Connected, c.Force = 2, 1, true, true }), "", ""},
		{"own last key", with(user(enrol.OpKeyRemove, "bob"), func(c *check) { c.KeysLeft = 0 }), enrol.CodeForce, "your last key"},
		{"own last key, forced", with(user(enrol.OpKeyRemove, "bob"), func(c *check) { c.KeysLeft, c.Force = 0, true }), "", ""},

		// An admin: everything, other admins included...
		{"admin: a token for another account", admin(enrol.OpTokenCreate, "bob"), "", ""},
		{"admin: an unbound token", admin(enrol.OpTokenCreate, ""), "", ""},
		{"admin: lists every token", admin(enrol.OpTokenList, "alice"), "", ""},
		{"admin: lists accounts", admin(enrol.OpUserList, ""), "", ""},
		{"admin: removes bob", with(admin(enrol.OpUserRemove, "bob"), func(c *check) { c.KeysLeft = 0 }), "", ""},
		{"admin: removes another admin", with(admin(enrol.OpUserRemove, "carol"), func(c *check) { c.TargetAdmin, c.KeysLeft, c.OtherAdmins = true, 0, 1 }), "", ""},
		{"admin: another's last key, no -f", with(admin(enrol.OpKeyRemove, "bob"), func(c *check) { c.KeysLeft = 0 }), "", ""},
		{"admin: adds a key to bob", admin(enrol.OpKeyAdd, "bob"), "", ""},

		// ...except their own account, the last admin, and operator keys.
		{"admin: removes themselves", with(admin(enrol.OpUserRemove, "alice"), func(c *check) { c.Force = true }), enrol.CodeDenied,
			"cannot remove their own account"},
		{"admin: removes an account with operator keys", with(admin(enrol.OpUserRemove, "bob"), func(c *check) { c.OperatorDir, c.Force = "/etc/workspace/authorized_keys.d", true }),
			enrol.CodeDenied, "bob's keys are in /etc/workspace/authorized_keys.d, which this workspace does not write"},
		{"admin: removes the last admin's last key, even forced", with(admin(enrol.OpKeyRemove, "alice"), func(c *check) { c.TargetAdmin, c.KeysLeft, c.Force = true, 0, true }),
			enrol.CodeDenied, "alice is the last admin with a key"},
		{"admin: removes their last key with another admin left", with(admin(enrol.OpKeyRemove, "alice"), func(c *check) { c.TargetAdmin, c.KeysLeft, c.OtherAdmins, c.Force = true, 0, 1, true }), "", ""},
		{"admin: removes a spare key of the last admin", with(admin(enrol.OpKeyRemove, "alice"), func(c *check) { c.TargetAdmin, c.TargetKeys, c.KeysLeft = true, 2, 1 }), "", ""},
		{"admin: removes an admin who holds no key", with(admin(enrol.OpUserRemove, "carol"), func(c *check) { c.TargetAdmin, c.TargetKeys, c.KeysLeft = true, 0, 0 }), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := authorize(tc.c)
			switch {
			case tc.code == "" && got != nil:
				t.Fatalf("refused: %v", got)
			case tc.code == "":
				return
			case got == nil:
				t.Fatalf("allowed, want %s", tc.code)
			case got.Code != tc.code || !strings.Contains(got.Msg, tc.msg):
				t.Fatalf("got %s %q, want %s %q", got.Code, got.Msg, tc.code, tc.msg)
			case got.Fix == "":
				t.Errorf("%q has no fix", got.Msg)
			}
		})
	}
}

func TestADeniedTokenNamesTheOperatorsCommand(t *testing.T) {
	e := authorize(check{Op: enrol.OpTokenCreate, Caller: "bob", Target: "carol"})
	if e == nil || e.Fix != "ask an admin, or the operator: `remote-dockerd token create --account carol`" {
		t.Fatalf("got %v", e)
	}
}
