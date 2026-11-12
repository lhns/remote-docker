package config

import (
	"fmt"
	"strings"
)

// ParseOnOff reads a switch: on, true, 1 and yes, or off, false, 0 and no.
func ParseOnOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("%q is not on or off", s)
}

// onOff is a switch in its one spelling.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
