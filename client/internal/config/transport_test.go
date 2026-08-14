package config

// What `host` means, which is the one setting that says where a workspace is.
//
// The case that matters most is the first: a bare host has to keep meaning
// exactly what it meant before schemes existed, or every configuration already
// written changes meaning under its owner.

import (
	"strings"
	"testing"
)

func TestTransport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		want    Transport
		wantErr string
	}{{
		name: "a bare host is ssh on the default port, as it always was",
		cfg:  Config{Host: "dev.example"},
		want: Transport{Kind: TransportSSH, Host: "dev.example", Port: DefaultSSHPort},
	}, {
		name: "a bare host with a port",
		cfg:  Config{Host: "dev.example", Port: 2299},
		want: Transport{Kind: TransportSSH, Host: "dev.example", Port: 2299},
	}, {
		name: "ssh said explicitly",
		cfg:  Config{Host: "ssh://dev.example:2299"},
		want: Transport{Kind: TransportSSH, Host: "dev.example", Port: 2299},
	}, {
		name: "ssh with no port falls back to the setting",
		cfg:  Config{Host: "ssh://dev.example", Port: 2299},
		want: Transport{Kind: TransportSSH, Host: "dev.example", Port: 2299},
	}, {
		name: "wss defaults to 443, the way a URL does",
		cfg:  Config{Host: "wss://ws.example/tunnel"},
		want: Transport{
			Kind: TransportWSS, Host: "ws.example", Port: 443,
			URL: "wss://ws.example/tunnel",
		},
	}, {
		name: "ws defaults to 80",
		cfg:  Config{Host: "ws://inside:8080/tunnel"},
		want: Transport{
			Kind: TransportWS, Host: "inside", Port: 8080,
			URL: "ws://inside:8080/tunnel",
		},
	}, {
		name: "a WebSocket with no path gets the agent's default",
		cfg:  Config{Host: "wss://ws.example"},
		want: Transport{
			Kind: TransportWSS, Host: "ws.example", Port: 443,
			URL: "wss://ws.example" + DefaultWSPath,
		},
	}, {
		// Two ports, one workspace: a mistake, and resolving it silently is how
		// it survives to confuse somebody later.
		name:    "a port that contradicts the host is refused",
		cfg:     Config{Host: "wss://ws.example:8443/tunnel", Port: 2222},
		wantErr: "remove one",
	}, {
		name: "a port that agrees with the host is fine",
		cfg:  Config{Host: "ssh://dev.example:2299", Port: 2299},
		want: Transport{Kind: TransportSSH, Host: "dev.example", Port: 2299},
	}, {
		name:    "a machine is reached over ssh, so a WebSocket for one is refused",
		cfg:     Config{Host: "wss://ws.example/tunnel", Machine: &Machine{Backend: "wsl", Name: "rd"}},
		wantErr: "reached over ssh",
	}, {
		name:    "an unknown scheme names itself",
		cfg:     Config{Host: "https://ws.example/tunnel"},
		wantErr: `"https"`,
	}, {
		name:    "no host at all",
		cfg:     Config{},
		wantErr: "no host",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.Transport()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Transport() = %+v, want an error naming %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Transport(): %v", err)
			}
			if got != tc.want {
				t.Errorf("Transport() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// `inspect` prints this, so it has to say which way in is being used rather
// than just the host both kinds share.
func TestTransportString(t *testing.T) {
	ssh, err := Config{Host: "dev.example"}.Transport()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ssh.String(), "ssh://dev.example:2222"; got != want {
		t.Errorf("ssh transport prints %q, want %q", got, want)
	}

	ws, err := Config{Host: "wss://ws.example/tunnel"}.Transport()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ws.String(), "wss://ws.example/tunnel"; got != want {
		t.Errorf("wss transport prints %q, want %q", got, want)
	}
	if !ws.WebSocket() || ssh.WebSocket() {
		t.Error("WebSocket() does not tell the two apart")
	}
}
