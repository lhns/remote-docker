//go:build windows

package machine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

// serviceHyperV is the executor the service runs: the same PowerShell as the
// direct path, with the machines' files under the service's own directory
// instead of a user's profile. A service writing or deleting under a path a
// caller influences is an arbitrary write through a junction, so the path is a
// function of a validated name and this root and nothing else.
func serviceHyperV(root string) directHyperV {
	d := newDirectHyperV()
	d.dir = func(name string) (string, error) {
		if err := validBrokerName(name); err != nil {
			return "", err
		}
		return filepath.Join(root, "machines", name), nil
	}
	return d
}

// newServiceServer wires the server to the real pipe, group and directory.
func newServiceServer(root string) *brokerServer {
	var fetch sync.Mutex // one download at a time into the shared cache
	return newBrokerServer(serviceHyperV(root), filepath.Join(root, "machines.json"),
		pipeCaller, brokerGroupMembers,
		func(ctx context.Context, proxy string) (string, error) {
			fetch.Lock()
			defer fetch.Unlock()
			return ensureFlatcar(ctx, flatcarPinned, filepath.Join(root, "rootfs"), proxy, io.Discard)
		})
}

// RunBrokerService runs the Windows service, and returns when it is stopped.
// Started by the service manager and by nothing else.
func RunBrokerService() error {
	if ok, err := svc.IsWindowsService(); err != nil || !ok {
		return errors.New("this runs only as the " + BrokerServiceName + " service; the installer sets it up")
	}
	return svc.Run(BrokerServiceName, brokerService{})
}

type brokerService struct{}

func (brokerService) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	root, err := brokerRoot()
	if err == nil {
		err = ensureBrokerDirs(root)
	}
	var l interface {
		Close() error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	if err == nil {
		srv := newServiceServer(root)
		listener, lerr := listenBroker()
		if lerr != nil {
			err = lerr
		} else {
			l = listener
			go func() { defer close(done); _ = srv.Serve(ctx, listener) }()
		}
	}
	if err != nil {
		// A service has no stderr and the manager reports only that it
		// stopped, so the reason goes where an administrator can find it.
		_ = os.WriteFile(filepath.Join(os.TempDir(), "remote-docker-service.log"),
			[]byte(time.Now().Format(time.RFC3339)+" "+err.Error()+"\r\n"), 0o600)
		return true, 1
	}

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range requests {
		switch c.Cmd {
		case svc.Interrogate:
			status <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			cancel()
			_ = l.Close()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
			}
			return false, 0
		}
	}
	return false, 0
}
