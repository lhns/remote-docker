package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/lhns/remote-docker/agent/internal/ephemeral"
)

func TestPrintRuns(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var out bytes.Buffer
	printRuns(&out, []ephemeral.Entry{
		{Account: "bob", Client: "89abcdef", Port: 65533, State: ephemeral.Live, LastSeen: now.Add(-5 * time.Second)},
		{Account: "alice", Client: "4567cdef", Port: 65534, State: ephemeral.Cleaning, LastSeen: now.Add(-3 * time.Minute)},
		{Account: "alice", Client: "0123abcd", Port: 65535, State: ephemeral.Grace, LastSeen: now.Add(-90 * time.Second)},
	}, now)
	want := "" +
		"ACCOUNT          CLIENT   STATE    PORT  AGE\n" +
		"alice            0123abcd grace    65535 1m30s\n" +
		"alice            4567cdef cleaning 65534 3m0s\n" +
		"bob              89abcdef live     65533 5s\n"
	if out.String() != want {
		t.Errorf("printRuns =\n%s\nwant\n%s", out.String(), want)
	}

	out.Reset()
	printRuns(&out, nil, now)
	if out.String() != "no ephemeral runs\n" {
		t.Errorf("with no runs: %q", out.String())
	}
}
