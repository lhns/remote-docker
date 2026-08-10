// Package logx is the one log handler both binaries use, and the reason they
// can use log/slog without their output changing.
//
// slog's own TextHandler writes `time=... level=INFO msg="connected to ..."`.
// That is right for a service whose logs are parsed and wrong for both of these
// programs: the client's lines are read by a person watching
// `remote-docker start --foreground`, and the agent's are read by whoever runs
// `docker logs` on a workspace that is misbehaving. So the structure is
// internal (attributes, levels, With) and the rendering stays what it was.
//
// Ten packages each declared their own `Logger interface { Printf(...) }`,
// with a nil check and a logf shim apiece, and one of them spelled it as a func
// field instead. This is what replaced them.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// ComponentKey is the attribute naming which part is speaking. The agent
// renders it as a prefix, which is how its container log has always read.
const ComponentKey = "component"

// Handler renders a record as one line: an optional prefix, the message, then
// any attributes as key=value.
//
// No timestamp and no level, deliberately. `docker logs` stamps every line
// already, and a level on every line of a program whose entire output is
// progress and problems is noise that pushes the message off the right of the
// terminal.
type Handler struct {
	mu     *sync.Mutex
	out    io.Writer
	indent string
	prefix bool
	attrs  []slog.Attr
	group  string
}

// New builds a handler writing to out.
//
// indent is put before every line -- two spaces for the client, where the lines
// sit under a command's own output. prefix renders the component attribute as
// `[name] `, which is the agent's format.
func New(out io.Writer, indent string, prefix bool) *Handler {
	return &Handler{mu: &sync.Mutex{}, out: out, indent: indent, prefix: prefix}
}

// Logger is New wrapped as a *slog.Logger, which is what everything takes.
func Logger(out io.Writer, indent string, prefix bool) *slog.Logger {
	return slog.New(New(out, indent, prefix))
}

// Discard is a logger that writes nothing.
//
// This is what replaced eleven `if x.Log != nil` guards. A nil *slog.Logger
// panics rather than staying quiet, so every zero value that used to mean
// silence has to name this instead, which is a fair trade for never writing
// the check again, but it IS the one thing to remember when adding a field.
func Discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// Everything is logged. The levels carry intent for a future reader and a
// future handler, not a filter: dropping a line the user needed in order to
// diagnose something is a worse failure than printing one they did not.
func (h *Handler) Enabled(context.Context, slog.Level) bool { return true }

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(h.indent)

	// The component prefix comes from an attribute rather than a field on the
	// handler, so `logger.With("component", "daemons")` is all a subsystem
	// needs -- the same mechanism as any other attribute, not a parallel one.
	rest := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	component := ""
	for _, a := range h.attrs {
		if h.prefix && a.Key == ComponentKey {
			component = a.Value.String()
			continue
		}
		rest = append(rest, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		if h.prefix && a.Key == ComponentKey {
			component = a.Value.String()
			return true
		}
		rest = append(rest, a)
		return true
	})

	if component != "" {
		b.WriteString("[")
		b.WriteString(component)
		b.WriteString("] ")
	}
	b.WriteString(r.Message)

	for _, a := range rest {
		b.WriteString(" ")
		if h.group != "" {
			b.WriteString(h.group)
			b.WriteString(".")
		}
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(render(a.Value))
	}
	b.WriteString("\n")

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

// render quotes only what needs it. An error or a path with a space in it must
// still be readable at a glance, and `msg="no such file or directory"` reads
// worse than the bare text for the audience these lines have.
func render(v slog.Value) string {
	s := fmt.Sprint(v.Any())
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}

// WithGroup qualifies subsequent keys. Nothing uses it today; it is here
// because slog.Handler requires it and a handler that silently dropped a group
// would lose attributes rather than fail.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	if next.group != "" {
		next.group += "." + name
	} else {
		next.group = name
	}
	return &next
}
