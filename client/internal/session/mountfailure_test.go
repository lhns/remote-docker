package session

import (
	"strings"
	"testing"
)

func TestExplainMount(t *testing.T) {
	const volume = "rd-1a2b3c4d-0f9e8d7c6b5a4f3e"
	tests := []struct {
		name       string
		ourClient  string
		ourPort    int
		client     string
		port       int
		wantMsg    string
		wantFixHas string
		wantOK     bool
	}{
		{"another client", "99999999", 40000, "1a2b3c4d", 40123, "shared by another client (1a2b3c4d), and nothing is serving it", "from that client", true},
		{"another client on the same port", "99999999", 40123, "1a2b3c4d", 40123, "another client (1a2b3c4d)", "from that client", true},
		{"our volume, another port", "1a2b3c4d", 40000, "1a2b3c4d", 40123, "made for tunnel port 40123, and this session's is 40000", "--force-recreate", true},
		{"our volume, our port", "1a2b3c4d", 40123, "1a2b3c4d", 40123, "not answering on port 40123", "docker remote restart", true},
		{"our port unknown", "1a2b3c4d", 0, "1a2b3c4d", 40123, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, fix, ok := explainMount(tt.ourClient, tt.ourPort, volume, tt.client, tt.port)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !strings.Contains(msg, tt.wantMsg) || !strings.Contains(fix, tt.wantFixHas) {
				t.Errorf("msg = %q, fix = %q; want %q and %q", msg, fix, tt.wantMsg, tt.wantFixHas)
			}
			if ok && !strings.HasPrefix(msg, "volume "+volume+": ") {
				t.Errorf("message %q does not name the volume", msg)
			}
		})
	}
}
