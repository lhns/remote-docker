package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/daemons"
	"github.com/lhns/remote-docker/agent/internal/ephemeral"
	"github.com/lhns/remote-docker/agent/internal/sshd"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core/workspace"
)

func TestAnUnsetMetricsAddrOpensNoListener(t *testing.T) {
	ln, err := listenMetrics("")
	if ln != nil || err != nil {
		t.Errorf("listenMetrics(\"\") = %v, %v; want no listener", ln, err)
	}
}

func TestABadMetricsAddrNamesTheVariable(t *testing.T) {
	_, err := listenMetrics("not an address")
	if err == nil || !strings.Contains(err.Error(), envMetricsAddr) {
		t.Errorf("err = %v, want one naming %s", err, envMetricsAddr)
	}
}

// Every metric the agent exposes, as a scrape of a fresh agent shows it.
func TestAgentMetricsExposition(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	mapping := workspace.DefaultMapping()
	server, err := sshd.New(sshd.Config{
		Accounts: accounts.New([]string{t.TempDir()}, "", t.TempDir(), mapping, nil, nil),
		Mapping:  mapping,
		HostKeys: []ssh.Signer{signer},
	})
	if err != nil {
		t.Fatal(err)
	}

	m := newAgentMetrics()
	runs := &ephemeral.Registry{Ports: &accounts.Ports{Mapping: mapping}, Refused: m.runsRefused}
	m.gauges(runs, map[string]bool{"ci": true}, server, &daemons.Manager{})

	var b strings.Builder
	if _, err := m.reg.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	var names []string
	for line := range strings.SplitSeq(b.String(), "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			names = append(names, name)
		}
	}
	want := []string{
		"remote_docker_account_daemons gauge",
		"remote_docker_ephemeral_objects_kept_total counter",
		"remote_docker_ephemeral_objects_removed_total counter",
		"remote_docker_ephemeral_run_ports gauge",
		"remote_docker_ephemeral_runs gauge",
		"remote_docker_ephemeral_runs_refused_total counter",
		"remote_docker_ephemeral_sweeps_total counter",
		"remote_docker_ssh_connections gauge",
		"remote_docker_token_limiter_rejections_total counter",
		"remote_docker_token_redemptions_total counter",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Errorf("metrics:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range []string{
		`remote_docker_ephemeral_runs{account="ci",state="cleaning"} 0`,
		`remote_docker_ephemeral_runs{account="ci",state="grace"} 0`,
		`remote_docker_ephemeral_runs{account="ci",state="live"} 0`,
		`remote_docker_ephemeral_runs_refused_total{reason="duplicate"} 0`,
		`remote_docker_token_redemptions_total{outcome="pending"} 0`,
		"remote_docker_account_daemons 0",
		"remote_docker_ephemeral_run_ports 0",
	} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("no line %q in:\n%s", line, b.String())
		}
	}
}
