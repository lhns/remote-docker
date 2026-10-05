//go:build windows

package machine

import (
	"os/exec"
	"strings"
	"testing"
)

// Every script reaches powershell.exe as one command-line argument, so a quote
// or a newline that does not survive Go's escaping and PowerShell's parsing
// fails only there. Parsed and not run: no runner has Hyper-V.
func TestPSScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("no powershell.exe")
	}
	for name, script := range map[string]string{
		"add":    psAddKVP("rd-dev"),
		"remove": psRemoveKVP("rd-dev"),
		"rm":     psRemoveVM("rd-dev", `C:\m`),
		"new":    psNewVM("rd-dev", `C:\m\disk.vhdx`, `C:\m`, Spec{Name: "dev", CPUs: 2, MemoryMB: 2048}),
	} {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			"$null = { "+psScript(script)+" }; 'parsed'")
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "parsed" {
			t.Errorf("%s does not parse: %v\n%s", name, err, out)
		}
	}
}
