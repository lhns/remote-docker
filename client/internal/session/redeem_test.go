package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/core/enrol"
)

// tokenWorkspace speaks the redeem as ADR 0051 has it, or as a workspace that
// predates it: x/crypto directly, since an old agent cannot be built here.
type tokenWorkspace struct {
	addr    net.Addr
	hostKey ssh.PublicKey

	accept bool // accept the token login
	banner bool // send the enrolment banner
	silent bool // read the request and never answer
	got    chan enrol.RedeemRequest
	login  chan string
	reply  enrol.RedeemReply
}

func startTokenWorkspace(t *testing.T, w *tokenWorkspace) *tokenWorkspace {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	w.hostKey = signer.PublicKey()
	w.got = make(chan enrol.RedeemRequest, 1)
	w.login = make(chan string, 4)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			w.login <- c.User()
			if w.accept {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("no")
		},
		BannerCallback: func(ssh.ConnMetadata) string {
			if w.banner {
				return enrol.Banner
			}
			return ""
		},
	}
	cfg.AddHostKey(signer)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w.addr = l.Addr()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go w.serve(conn, cfg)
		}
	}()
	return w
}

func (w *tokenWorkspace) serve(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range chReqs {
				var payload struct{ Command string }
				_ = ssh.Unmarshal(req.Payload, &payload)
				ok := req.Type == "exec" && payload.Command == enrol.RedeemCommand
				_ = req.Reply(ok, nil)
				if !ok {
					continue
				}
				var got enrol.RedeemRequest
				_ = enrol.ReadJSON(ch, &got)
				w.got <- got
				if w.silent {
					return
				}
				_ = json.NewEncoder(ch).Encode(w.reply)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
				return
			}
		}()
	}
}

func (w *tokenWorkspace) config(t *testing.T) config.Config {
	t.Helper()
	host, port, err := net.SplitHostPort(w.addr.String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(port)
	return config.Config{Host: host, Port: n}
}

func (w *tokenWorkspace) invite(t *testing.T, account string) enrol.Invite {
	t.Helper()
	id, secret, err := enrol.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return enrol.Invite{URL: "ssh://" + w.addr.String(), HostKey: ssh.FingerprintSHA256(w.hostKey),
		Account: account, Token: id + "." + secret}
}

func withState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("REMOTE_DOCKER_STATE_DIR", dir)
	return dir
}

func TestRedeemSendsTheSecretAndRecordsTheHostKey(t *testing.T) {
	withState(t)
	w := startTokenWorkspace(t, &tokenWorkspace{accept: true, banner: true,
		reply: enrol.RedeemReply{Account: "alice", Created: true}})
	invite := w.invite(t, "alice")

	reply, err := Redeem(t.Context(), w.config(t), invite, "Alice")
	if err != nil || reply.Account != "alice" || !reply.Created {
		t.Fatalf("Redeem = %+v, %v", reply, err)
	}
	id, secret, _ := enrol.ParseToken(invite.Token)
	if login := <-w.login; login != enrol.LoginPrefix+id {
		t.Errorf("logged in as %q", login)
	}
	if got := <-w.got; got.Secret != secret || got.Account != "Alice" || got.Comment == "" {
		t.Errorf("sent %+v", got)
	}
	known, err := os.ReadFile(config.KnownHostsPath())
	if err != nil || !strings.Contains(string(known), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(w.hostKey)))) {
		t.Errorf("known_hosts after a redeem: %q, %v", known, err)
	}
}

func TestRedeemRefusesAHostKeyTheInviteDoesNotName(t *testing.T) {
	withState(t)
	w := startTokenWorkspace(t, &tokenWorkspace{accept: true, banner: true})
	invite := w.invite(t, "alice")
	invite.HostKey = "SHA256:somebody-else"

	_, err := Redeem(t.Context(), w.config(t), invite, "")
	if err == nil || !strings.Contains(err.Error(), "the token is for SHA256:somebody-else") {
		t.Fatalf("Redeem = %v", err)
	}
	requireFix(t, err)
	if data, _ := os.ReadFile(config.KnownHostsPath()); len(data) != 0 {
		t.Errorf("an unpinned host key was recorded: %q", data)
	}
}

// known_hosts still refuses a key that changed, even one the invite pins.
func TestThePinDoesNotOverrideAChangedHostKey(t *testing.T) {
	withState(t)
	w := startTokenWorkspace(t, &tokenWorkspace{accept: true, banner: true})
	impostor := startTokenWorkspace(t, &tokenWorkspace{accept: true, banner: true})

	if _, err := Redeem(t.Context(), w.config(t), w.invite(t, "alice"), ""); err != nil {
		t.Fatal(err)
	}
	// The impostor's key at w's address, pinned by an invite of its own.
	pinned := pinnedHostKey(ssh.FingerprintSHA256(impostor.hostKey), mustKnownHosts(t), new(error))
	if err := pinned(w.addr.String(), w.addr, impostor.hostKey); err == nil || !strings.Contains(err.Error(), "CHANGED") {
		t.Errorf("a changed host key was accepted because the invite pinned it: %v", err)
	}
}

func TestATokenLoginRefusedWithTheBannerIsARefusedToken(t *testing.T) {
	withState(t)
	w := startTokenWorkspace(t, &tokenWorkspace{banner: true})
	_, err := Redeem(t.Context(), w.config(t), w.invite(t, "alice"), "")
	if err != enrol.Refused {
		t.Fatalf("Redeem = %v, want enrol.Refused", err)
	}
}

func TestATokenLoginRefusedWithoutTheBannerIsAnOldWorkspace(t *testing.T) {
	withState(t)
	w := startTokenWorkspace(t, &tokenWorkspace{})
	_, err := Redeem(t.Context(), w.config(t), w.invite(t, "alice"), "")
	if !errors.Is(err, ErrPredatesTokens) {
		t.Fatalf("Redeem = %v, want ErrPredatesTokens", err)
	}
}

func TestARefusalInTheReplyIsReturned(t *testing.T) {
	withState(t)
	want := &enrol.Error{Code: enrol.CodeName, Msg: "the account alice already exists", Fix: "choose another name with --user"}
	w := startTokenWorkspace(t, &tokenWorkspace{accept: true, banner: true, reply: enrol.RedeemReply{Error: want}})
	_, err := Redeem(t.Context(), w.config(t), w.invite(t, ""), "alice")
	var got *enrol.Error
	if !errors.As(err, &got) || *got != *want {
		t.Fatalf("Redeem = %v", err)
	}
}

// A workspace that takes the request and never answers must not hold the
// redeem past its context.
func TestRedeemHonoursItsContextWhileWaitingForTheAnswer(t *testing.T) {
	withState(t)
	w := startTokenWorkspace(t, &tokenWorkspace{accept: true, banner: true, silent: true})
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Redeem(ctx, w.config(t), w.invite(t, "alice"), "")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Redeem = %v, want the context's deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Redeem is still waiting for an answer its context gave up on")
	}
}

func mustKnownHosts(t *testing.T) ssh.HostKeyCallback {
	t.Helper()
	cb, err := hostKeyRule(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return cb
}

func requireFix(t *testing.T, err error) {
	t.Helper()
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "  fix: ") {
		t.Errorf("want one line of diagnosis and one fix line, got %q", err)
	}
}
