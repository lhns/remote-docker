package nfsserve

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	nfs "github.com/willscott/go-nfs"

	"github.com/lhns/remote-docker/internal/logx"
)

// capture reads back what was actually rendered, rather than what was passed
// to a Printf. The point of these tests is which go-nfs messages reach a user
// at all, so the assertion belongs at the far end.
type capture struct{ buf bytes.Buffer }

func (c *capture) logger() *slog.Logger { return slog.New(logx.New(&c.buf, "", false)) }

func (c *capture) lines() []string {
	out := strings.Split(strings.TrimSpace(c.buf.String()), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// The NFS_ACL probe is the first thing a Linux client does on every v3 mount.
// go-nfs logs the correct refusal at ERROR, so the first line a user ever saw
// from a session was a red herring they had no way to judge.
func TestACLProbeIsNotReported(t *testing.T) {
	c := &capture{}
	l := &nfsLogger{log: c.logger()}

	l.Errorf("No handler for %d.%d", 100227, 0)

	if len(c.lines()) != 0 {
		t.Errorf("reported the ACL probe: %v", c.lines())
	}
}

// A genuine fault must still surface, or silencing the noise would silence
// everything.
func TestRealErrorsAreReported(t *testing.T) {
	c := &capture{}
	l := &nfsLogger{log: c.logger()}

	l.Errorf("No handler for %d.%d", 100003, 42)
	l.Warnf("something is wrong")

	if len(c.lines()) != 2 {
		t.Fatalf("reported %v, want both messages", c.lines())
	}
	for _, want := range []string{"100003", "something is wrong"} {
		found := false
		for _, line := range c.lines() {
			if strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q was not reported: %v", want, c.lines())
		}
	}
}

// go-nfs logs per-request detail at Info, which is a firehose in a CLI.
func TestChatterIsDropped(t *testing.T) {
	c := &capture{}
	l := &nfsLogger{log: c.logger()}

	l.Infof("serving a request")
	l.Debugf("detail")
	l.Tracef("more detail")
	l.Print("print")

	if len(c.lines()) != 0 {
		t.Errorf("forwarded low-level chatter: %v", c.lines())
	}
}

// go-nfs's init calls ParseLevel with whatever LOG_LEVEL holds, so it must not
// panic on nonsense.
func TestParseLevel(t *testing.T) {
	l := &nfsLogger{}
	for in, want := range map[string]nfs.LogLevel{
		"error": nfs.ErrorLevel, "WARN": nfs.WarnLevel, " info ": nfs.InfoLevel,
	} {
		got, err := l.ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := l.ParseLevel("loud"); err == nil {
		t.Error("ParseLevel accepted nonsense")
	}
}

// A nil logger must not panic: SetLogger is called with whatever the session
// was given, and that can be nil.
func TestNilLoggerIsSafe(t *testing.T) {
	l := &nfsLogger{}
	l.Errorf("No handler for %d.%d", 100003, 42)
	l.Warn("x")
}
