package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

// %q is Go escaping, not JSON: a control character came out as \x1b, which
// the Docker CLI cannot parse.
func TestWriteErrorIsValidJSON(t *testing.T) {
	const msg = "dial failed: \x1b[31mred\x1b[0m \"quoted\"\n"

	var buf bytes.Buffer
	writeError(&buf, errors.New(msg))

	resp, err := http.ReadResponse(bufio.NewReader(&buf), nil)
	if err != nil {
		t.Fatalf("reading response: %v\n%q", err, buf.String())
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusInternalServerError || !resp.Close {
		t.Errorf("status %d, close %v; want 500 and Connection: close", resp.StatusCode, resp.Close)
	}
	var got struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%q", err, body)
	}
	if got.Message != msg {
		t.Errorf("message = %q, want %q", got.Message, msg)
	}
}
