//go:build !windows

package machine

import (
	"context"
	"errors"
	"net"
)

// The Hyper-V service exists on Windows only (ADR 0055). These keep the
// client building elsewhere and say so when asked.

var errBrokerPlatform = errors.New("the Hyper-V service exists on Windows only")

func dialBroker(context.Context) (net.Conn, error)       { return nil, errBrokerPlatform }
func BrokerInstalled() (bool, error)                     { return false, nil }
func RunBrokerService() error                            { return errBrokerPlatform }
func BrokerSetup(string) error                           { return errBrokerPlatform }
func BrokerTeardown(context.Context) error               { return errBrokerPlatform }
func BrokerPurgeLocal(context.Context) ([]string, error) { return nil, errBrokerPlatform }
