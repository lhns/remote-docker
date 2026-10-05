package enrol

import (
	"encoding/base64"
	"strings"
	"testing"
)

func newTestToken(t *testing.T) string {
	t.Helper()
	id, secret, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return id + "." + secret
}

func TestATokenRoundTrips(t *testing.T) {
	token := newTestToken(t)
	id, secret, err := ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken(%q): %v", token, err)
	}
	if id+"."+secret != token || !ValidID(id) {
		t.Errorf("parsed %q into %q and %q", token, id, secret)
	}
}

func TestMalformedTokensAreRefused(t *testing.T) {
	good := newTestToken(t)
	id, secret, _ := strings.Cut(good, ".")
	for _, s := range []string{
		"", "-", id, id + ".", "." + secret,
		strings.ToUpper(id) + "." + secret,
		"../../etc." + secret,
		id + "." + secret + "A",
		id + "x." + secret,
	} {
		if _, _, err := ParseToken(s); err == nil {
			t.Errorf("ParseToken(%q) accepted it", s)
		}
	}
}

func TestAnInviteRoundTripsAsOneShellSafeWord(t *testing.T) {
	in := Invite{URL: "wss://ws.example/", HostKey: "SHA256:abc", Account: "alice", Token: newTestToken(t)}
	s := in.String()
	if !strings.HasPrefix(s, InvitePrefix) || strings.ContainsAny(s, " \t\n'\"$`\\|&;<>()*?=") {
		t.Errorf("not one shell-safe word: %q", s)
	}
	out, err := ParseInvite(s)
	if err != nil {
		t.Fatalf("ParseInvite: %v", err)
	}
	if out != in {
		t.Errorf("got %+v, want %+v", out, in)
	}
	if _, err := ParseInvite("  " + s + "\n"); err != nil {
		t.Errorf("surrounding whitespace was refused: %v", err)
	}
}

// `--token -` is not a way to read stdin: there is none, and it is refused as
// what it is.
func TestMalformedInvitesAreRefused(t *testing.T) {
	encode := func(json string) string {
		return InvitePrefix + base64.RawURLEncoding.EncodeToString([]byte(json))
	}
	token := newTestToken(t)
	for name, s := range map[string]string{
		"dash":         "-",
		"empty":        "",
		"bare token":   token,
		"other prefix": "rdt2." + strings.TrimPrefix(Invite{URL: "ws://x", HostKey: "SHA256:a", Token: token}.String(), InvitePrefix),
		"not base64":   InvitePrefix + "!!!",
		"not json":     encode("nope"),
		"no url":       encode(`{"k":"SHA256:a","t":"` + token + `"}`),
		"no host key":  encode(`{"u":"ws://x","t":"` + token + `"}`),
		"bad token":    encode(`{"u":"ws://x","k":"SHA256:a","t":"nope"}`),
	} {
		if _, err := ParseInvite(s); err != ErrMalformed {
			t.Errorf("%s: ParseInvite(%q) = %v, want ErrMalformed", name, s, err)
		}
	}
}

func TestTheRefusalNamesNoneOfItsCauses(t *testing.T) {
	lines := strings.Split(Refused.Error(), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "  fix: ") {
		t.Fatalf("want one diagnosis and one fix line, got %q", Refused.Error())
	}
	if !strings.Contains(lines[0], "unknown, used or expired") {
		t.Errorf("the refusal must name all three causes: %q", lines[0])
	}
}
