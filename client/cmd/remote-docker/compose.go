package main

// `docker compose`, embedded (ADR 0009).

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/docker/cli/cli/command"
	composecmd "github.com/docker/compose/v5/cmd/compose"
	"github.com/docker/compose/v5/cmd/prompt"
	"github.com/docker/compose/v5/pkg/compose"
	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/client/internal/proxy"
)

// installCompose adds the compose tree as docker's plugin harness would.
// Not plugin.Run itself, which would initialise a second CLI over ours.
// client names this run when the workspace made it ephemeral, and is nil when
// the invocation is not ours.
func installCompose(cmd *cobra.Command, dockerCli *command.DockerCli, client func() string) {
	backend := &composecmd.BackendOptions{
		Options: []compose.Option{
			// Without it `compose down --remove-orphans` cannot confirm.
			compose.WithPrompt(prompt.NewPrompt(dockerCli.In(), dockerCli.Out()).Confirm),
		},
	}

	sub := composecmd.RootCommand(dockerCli, backend)
	sub.AddCommand(composecmd.HooksCommand())
	if client != nil {
		defaultProjectName(sub, client)
	}
	cmd.AddCommand(sub)
}

// defaultProjectName gives an ephemeral run's projects a name of their own,
// `<name>-<client id>`, unless -p or COMPOSE_PROJECT_NAME chose one: runs of
// one account share a daemon, so the same compose file from two runs would
// otherwise be one project (ADR 0050). The suffix is also what the workspace's
// cleanup finds the run's networks by.
func defaultProjectName(sub *cobra.Command, client func() string) {
	pre := sub.PersistentPreRunE
	sub.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// First: compose's own reads .env's COMPOSE_ variables into the
		// environment, so one set there counts as set.
		if pre != nil {
			if err := pre(cmd, args); err != nil {
				return err
			}
		}
		flags := sub.Flags()
		if flags.Changed("project-name") || os.Getenv(composecmd.ComposeProjectName) != "" {
			return nil
		}
		id := client()
		if id == "" {
			return nil // a machine, which keeps compose's own name
		}
		name := id
		if base := projectBase(sub); base != "" {
			name = base + "-" + id
		}
		return flags.Set("project-name", name)
	}
}

// projectBase is the name compose would give the project: the file's `name:`
// or its directory's. "" when neither resolves, and compose then reports why.
func projectBase(sub *cobra.Command) string {
	flags := sub.Flags()
	files, _ := flags.GetStringArray("file")
	envFiles, _ := flags.GetStringArray("env-file")
	dir, _ := flags.GetString("project-directory")
	if dir == "" {
		dir, _ = flags.GetString("workdir")
	}

	opts, err := cli.NewProjectOptions(files,
		cli.WithWorkingDirectory(dir),
		cli.WithOsEnv,
		cli.WithEnvFiles(envFiles...),
		cli.WithDotEnv,
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
	)
	if err != nil {
		return ""
	}
	if model, err := opts.LoadModel(context.Background()); err == nil {
		if name, ok := model["name"].(string); ok && name != "" {
			return name
		}
	}
	wd, err := opts.GetWorkingDir()
	if err != nil {
		return ""
	}
	return loader.NormalizeProjectName(filepath.Base(wd))
}

// clientTimeout bounds asking the session for its client, which connects.
const clientTimeout = time.Minute

// sessionClient asks the session serving endpoint which client it is. ""
// when it cannot say, and compose then names the project itself.
func sessionClient(endpoint string) func() string {
	if endpoint == "" {
		return nil
	}
	return func() string {
		var c proxy.Client
		if controlWithin(clientTimeout, endpoint, http.MethodGet, "client", &c) != nil {
			return ""
		}
		return c.Client
	}
}
