//go:build !linux

package main

import "errors"

// The probe measures Linux inotify semantics inside the workspace container.
// It exists as a non-Linux stub only so `go build ./...` and the
// cross-compile matrix stay green on the development machine, which has
// neither Docker nor a Linux kernel.
func watch(string, func(mask, name string)) (func(), error) {
	return nil, errors.New("watchprobe measures inotify and only runs on Linux")
}
