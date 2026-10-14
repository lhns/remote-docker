package session

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// A dial that meets the wait's deadline says so before the wait's context
// does, in whichever spelling the layer under it uses.
func TestIsTimeoutKnowsEveryDeadlineSpelling(t *testing.T) {
	var netTimeout net.Error = timeoutError{}
	for name, err := range map[string]error{
		"context": fmt.Errorf("dial: %w", context.DeadlineExceeded),
		"os":      fmt.Errorf("read: %w", os.ErrDeadlineExceeded),
		"net":     fmt.Errorf("tunnel: dialling: %w", &net.OpError{Op: "dial", Err: netTimeout}),
	} {
		if !isTimeout(err) {
			t.Errorf("%s: %v is not a timeout", name, err)
		}
	}
	if isTimeout(errors.New("connection refused")) {
		t.Error("a refusal is not a timeout")
	}
}
