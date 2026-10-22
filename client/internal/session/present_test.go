package session

import (
	"errors"
	"testing"

	"github.com/lhns/remote-docker/dircache"
)

// Presence is asked only of an agent that announced Carry; without it the answer
// is an error, which the caller reads as "keep everything".
func TestPresentIsAskedOnlyOfAnAgentThatAnnouncedCarry(t *testing.T) {
	stream := newScripted(`{"x":["/a"]}` + "\n")
	c := channelOver(stream)
	if _, err := c.Present(t.Context(), "/m/1111111111111111", []string{"/a", "/b"}); !errors.Is(err, dircache.ErrCannotPresent) {
		t.Errorf("without Carry: %v, want dircache.ErrCannotPresent", err)
	}
	if len(stream.written) != 0 {
		t.Errorf("a request was sent to an agent that cannot answer: %s", stream.written)
	}

	c.carry = true
	here, err := c.Present(t.Context(), "/m/1111111111111111", []string{"/a", "/b"})
	if err != nil || len(here) != 1 || here[0] != "/a" {
		t.Errorf("Present = %v, %v; want [/a]", here, err)
	}
}

// A file the workspace names as unreadable reaches write-back as one, so it is
// set aside by name instead of silently never arriving.
func TestAPullHandsOnWhatTheWorkspaceCouldNotRead(t *testing.T) {
	c := channelOver(newScripted(`{"k":["/bad"]}` + "\n"))
	c.carry = true
	var got []dircache.File
	err := c.Pull(t.Context(), "/m/1111111111111111", []string{"/bad"}, func(f dircache.File) error {
		got = append(got, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/bad" || got[0].Unreadable == "" || got[0].Body != nil {
		t.Errorf("handed on %+v, want /bad as unreadable", got)
	}
}
