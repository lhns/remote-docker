package nfsserve

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	nfs "github.com/willscott/go-nfs"
)

// go-nfs logs to stderr through a package-level logger of its own, at Info by
// default. Left alone it writes straight past the client's own logging and
// onto the user's terminal, and the first thing it says on every single mount
// is:
//
//	[ERROR] No handler for 100227.0
//
// Program 100227 is NFS_ACL and procedure 0 is NULL, so that line is the Linux
// NFS client asking "do you support ACLs?" and being correctly told no. It is
// a routine probe reported as a failure, and it is alarming precisely when a
// user is least able to judge it -- the first time they run `shell`.
//
// The mount options carry `noacl` now, which stops the probe at source. This
// stays as well, because the probe is only the loudest example: go-nfs's
// default logger is a stderr firehose we do not control, and a file server
// embedded in a CLI has no business writing to the terminal on its own.

// SetLogger routes go-nfs's logging into the client's, once.
//
// Package-level because go-nfs's logger is package-level; there is nothing
// per-server to attach it to.
func SetLogger(log *slog.Logger) {
	loggerOnce.Do(func() { nfs.SetLogger(&nfsLogger{log: log}) })
}

var loggerOnce sync.Once

// nfsLogger adapts go-nfs's logger to ours.
//
// Everything below Warn is dropped: go-nfs logs per-request detail at Info,
// which is useful when debugging the NFS server and noise in every other
// circumstance. Warn and above are forwarded, so a real server fault still
// surfaces.
type nfsLogger struct {
	log   *slog.Logger
	mu    sync.Mutex
	level nfs.LogLevel
}

// benign are messages that describe correct behaviour in alarming words.
// Matched on a prefix rather than a whole line, because the tail carries
// numbers that vary.
var benign = []string{
	// The NFS_ACL sideband probe. `noacl` in the mount options should prevent
	// it, but a mount made by hand, or by an older workspace whose options
	// predate that, will still ask.
	"No handler for 100227",
}

func (l *nfsLogger) forward(level nfs.LogLevel, msg string) {
	if level > nfs.WarnLevel {
		return
	}
	for _, prefix := range benign {
		if strings.Contains(msg, prefix) {
			return
		}
	}
	if l.log != nil {
		l.log.Warn("nfs: " + msg)
	}
}

func (l *nfsLogger) SetLevel(level nfs.LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

func (l *nfsLogger) GetLevel() nfs.LogLevel {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// ParseLevel exists because go-nfs's init reads LOG_LEVEL from the
// environment and calls it. Returning an error for anything unrecognised
// leaves the level alone, which is what we want.
func (l *nfsLogger) ParseLevel(level string) (nfs.LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "panic":
		return nfs.PanicLevel, nil
	case "fatal":
		return nfs.FatalLevel, nil
	case "error":
		return nfs.ErrorLevel, nil
	case "warn", "warning":
		return nfs.WarnLevel, nil
	case "info":
		return nfs.InfoLevel, nil
	case "debug":
		return nfs.DebugLevel, nil
	case "trace":
		return nfs.TraceLevel, nil
	}
	return nfs.InfoLevel, fmt.Errorf("nfsserve: unknown log level %q", level)
}

func (l *nfsLogger) Panic(args ...any) { l.forward(nfs.PanicLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Fatal(args ...any) { l.forward(nfs.FatalLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Error(args ...any) { l.forward(nfs.ErrorLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Warn(args ...any)  { l.forward(nfs.WarnLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Info(args ...any)  { l.forward(nfs.InfoLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Debug(args ...any) { l.forward(nfs.DebugLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Trace(args ...any) { l.forward(nfs.TraceLevel, fmt.Sprint(args...)) }
func (l *nfsLogger) Print(args ...any) { l.forward(nfs.InfoLevel, fmt.Sprint(args...)) }

func (l *nfsLogger) Panicf(f string, a ...any) { l.forward(nfs.PanicLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Fatalf(f string, a ...any) { l.forward(nfs.FatalLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Errorf(f string, a ...any) { l.forward(nfs.ErrorLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Warnf(f string, a ...any)  { l.forward(nfs.WarnLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Infof(f string, a ...any)  { l.forward(nfs.InfoLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Debugf(f string, a ...any) { l.forward(nfs.DebugLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Tracef(f string, a ...any) { l.forward(nfs.TraceLevel, fmt.Sprintf(f, a...)) }
func (l *nfsLogger) Printf(f string, a ...any) { l.forward(nfs.InfoLevel, fmt.Sprintf(f, a...)) }
