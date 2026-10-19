package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseDuration reads a Go duration such as 36h, or whole days such as 7d,
// which time.ParseDuration has no unit for. A negative value is refused.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 || n > 36500 {
			return 0, fmt.Errorf("%q is not a whole number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	return d, nil
}
