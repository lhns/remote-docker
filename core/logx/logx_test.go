package logx

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The rendered line is the whole point of this package, so it is pinned.
//
// A migration this mechanical gets exactly one thing wrong without noticing:
// the output. `go build` proves nothing about it, and the two audiences --
// somebody watching `start --foreground`, somebody reading `docker logs` on a
// workspace, would both be handed slog's `time=... level=INFO msg="..."`
// instead of the prose they have always had.
func TestTheClientsFormatIsUnchanged(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(New(&out, "  ", false))

	log.Info("connected to alice@dev.example")

	if got, want := out.String(), "  connected to alice@dev.example\n"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

// The agent's lines say which part spoke, which is what makes a workspace log
// readable when three subsystems are talking at once.
func TestTheAgentsPrefixComesFromTheComponent(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(New(&out, "", true)).With(ComponentKey, "daemons")

	log.Info("starting a daemon for alice")

	if got, want := out.String(), "[daemons] starting a daemon for alice\n"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}

// Attributes follow the message as key=value, and only what needs quoting gets
// it: an error message with spaces in it is the common case and must stay
// readable.
func TestAttributesFollowTheMessage(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(New(&out, "", false))

	log.Info("collecting unused share volumes",
		"err", errors.New("no such file or directory"), "count", 3)

	got := out.String()
	if !strings.Contains(got, `err="no such file or directory"`) {
		t.Errorf("an error with spaces was not quoted: %q", got)
	}
	if !strings.Contains(got, "count=3") {
		t.Errorf("a plain value was quoted or lost: %q", got)
	}
}

// Discard is what replaced eleven nil checks, so it must genuinely write
// nothing rather than merely not crashing.
func TestDiscardWritesNothing(t *testing.T) {
	log := Discard()
	log.Info("this must not appear", "anywhere", true)
	log.Error("nor this")
	// Reaching here without a panic is most of the claim; the rest is that
	// nothing has anywhere to go, which slog.DiscardHandler guarantees.
}

// Every level renders the same way, because filtering is not this handler's
// job: dropping the line somebody needed to diagnose a problem is worse than
// printing one they did not.
func TestLevelsAreNotFilteredOrRendered(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(New(&out, "", false))

	log.Debug("a")
	log.Info("b")
	log.Warn("c")
	log.Error("d")

	if got, want := out.String(), "a\nb\nc\nd\n"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}
