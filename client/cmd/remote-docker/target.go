package main

// Which daemon a docker command talks to, and whether we have any business
// arranging it.
//
// The endpoint we serve is reached through a docker context, one per workspace
// (ADR 0018), and docker resolves a target in a fixed order:
//
//	--host / -H  >  DOCKER_HOST  >  --context / DOCKER_CONTEXT  >  current context
//
// Setting DOCKER_HOST whenever it was empty put us at the top of that list and
// silently overrode everything below it. Two things were wrong and only one of
// them was ours to notice:
//
//   - `docker --context ci ps` reached the DEFAULT workspace, not ci;
//   - `docker --context desktop ps`, a context we never created, reached US
//     instead of Docker Desktop.
//
// The second is the one that matters. A machine may have real docker contexts,
// and a tool that quietly redirects them is worse than one that needs a prefix.

import (
	"os"
	"strings"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/proxy"
)

// target says what to do about a docker invocation.
type target struct {
	// workspace names the workspace to make a session for, or "" for the
	// configured default. Meaningless when ensure is false.
	workspace string

	// ensure is whether to make a session available at all.
	ensure bool

	// setHost is whether to point DOCKER_HOST at our endpoint. False whenever
	// a context is doing the pointing, because DOCKER_HOST would override it.
	setHost bool
}

// leaveAlone is the answer for an invocation aimed at something that is not
// ours: no session, no override, no opinion.
var leaveAlone = target{}

// lookups is everything decideTarget has to ask the outside world, so that it
// can be a pure function of its inputs and every branch below can be a row in
// a table.
type lookups struct {
	getenv         func(string) string
	currentContext func() string
	contextIsOurs  func(string) bool
	workspaceFor   func(string) string
	endpointIsOurs func(string) bool
}

// realLookups asks the machine.
func realLookups() lookups {
	return lookups{
		getenv:         os.Getenv,
		currentContext: currentDockerContext,
		contextIsOurs:  contextIsOurs,
		workspaceFor:   workspaceForContext,
		endpointIsOurs: endpointIsOurs,
	}
}

// decideTarget reads the invocation and says what to arrange.
//
// Pure given its lookups, and that is deliberate: this runs before cobra has
// parsed anything, so it cannot be observed from a test any other way, and the
// case it exists for is the one where doing nothing is the right answer.
func decideTarget(args []string, look lookups) target {
	// An explicit -H or --host outranks everything, including a context, and
	// says plainly that the user meant somewhere else.
	if _, ok := flagValue(args, "--host", "-H"); ok {
		return leaveAlone
	}

	// DOCKER_HOST naming something that is not our endpoint is the same
	// instruction by another route. Naming ours is not: that is what our own
	// printed value and our own context both say, and treating it as foreign
	// disabled starting a session and noticing a stale one.
	if host := look.getenv("DOCKER_HOST"); host != "" {
		if !look.endpointIsOurs(host) {
			return leaveAlone
		}
		return target{ensure: true}
	}

	// A context, whether named on the command line or in the environment.
	// Docker prefers the flag, so we do too.
	//
	// Whether one was ASKED FOR is tracked separately from what it was, because
	// `docker --context` with nothing after it is a request docker will reject,
	// and quietly picking the default behind it would open a session for a
	// command that cannot run.
	name, asked := flagValue(args, "--context", "-c")
	if !asked {
		if fromEnv := look.getenv("DOCKER_CONTEXT"); fromEnv != "" {
			name, asked = fromEnv, true
		}
	}
	if !asked {
		name = look.currentContext()
	} else if name == "" {
		return leaveAlone
	}

	switch {
	case name == "":
		// Nothing selected anywhere, so nothing is being overridden and the
		// configured default workspace is the only sensible target.
		return target{ensure: true, setHost: true}
	case look.contextIsOurs(name):
		// Ours, so make a session for THAT workspace and let docker read the
		// endpoint off the context. Setting DOCKER_HOST here would override
		// the very context we just agreed to honour.
		return target{workspace: look.workspaceFor(name), ensure: true}
	default:
		return leaveAlone
	}
}

// flagValue finds a flag's value in an unparsed argument list.
//
// Stops at the first non-flag word, which is the subcommand: `docker run -c 2`
// is a container flag and none of our business. Handles --flag=value and
// --flag value, and the shorthands docker gives these two.
func flagValue(args []string, long, short string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return "", false
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if name != long && name != short {
			// Skip a value that belongs to some other flag, or it reads as the
			// subcommand and ends the scan early.
			if !hasValue && valuedRootFlags[name] {
				i++
			}
			continue
		}
		if hasValue {
			return value, true
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
		// Named with nothing after it. Docker will reject it; what matters here
		// is that a context WAS asked for, so we do not go picking a default.
		return "", true
	}
	return "", false
}

// endpointIsOurs reports whether a DOCKER_HOST value is an endpoint this
// machine serves for some configured workspace.
//
// Any of them, not just the one that would resolve here: pointing DOCKER_HOST
// at ci's endpoint is still a request for a session, just not for the default.
func endpointIsOurs(host string) bool {
	for _, cfg := range eachWorkspace() {
		if host == proxy.DockerHost(endpointOf(cfg)) {
			return true
		}
	}
	return false
}

// workspaceForContext maps a docker context back to the workspace that owns
// it, or "" when nothing configured claims it.
func workspaceForContext(name string) string {
	for _, cfg := range eachWorkspace() {
		if cfg.ContextName() == name {
			return cfg.Name
		}
	}
	return ""
}

// eachWorkspace resolves every configured workspace, and the unnamed one.
//
// Errors are dropped rather than reported: this runs while the command tree is
// still being built, where there is nobody to report to, and a workspace that
// will not resolve simply cannot be the answer to either question above.
func eachWorkspace() []config.Config {
	file, err := config.Load("")
	if err != nil {
		return nil
	}
	names := file.Names()
	if len(names) == 0 {
		names = []string{""}
	}
	out := make([]config.Config, 0, len(names))
	for _, name := range names {
		cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
		if err != nil {
			continue
		}
		out = append(out, cfg)
	}
	return out
}
