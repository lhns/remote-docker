package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lhns/remote-docker/internal/client/config"
	"github.com/lhns/remote-docker/internal/client/proxy"
)

func newWorkspacesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "workspaces",
		Short: "List the configured workspaces",
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := config.Load("")
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			names := file.Names()
			if len(names) == 0 {
				if file.Host != "" {
					_, _ = fmt.Fprintf(out, "one unnamed workspace: %s@%s:%d\n",
						file.User, file.Host, file.Port)
					return nil
				}
				_, _ = fmt.Fprintf(out,
					"no workspaces configured.\nWrite %s:\n\n"+
						"    {\n"+
						"      \"workspaces\": {\n"+
						"        \"dev\": {\"host\": \"dev.example\", \"user\": \"alice\"},\n"+
						"        \"ci\":  {\"host\": \"ci.example\",  \"user\": \"alice\"}\n"+
						"      },\n"+
						"      \"default\": \"dev\"\n"+
						"    }\n",
					config.DefaultPath())
				return nil
			}

			_, _ = fmt.Fprintf(out, "%-14s %-30s %s\n", "NAME", "WORKSPACE", "ENDPOINT")
			for _, name := range names {
				cfg, err := config.Resolve(config.Overrides{Workspace: name}, "")
				if err != nil {
					_, _ = fmt.Fprintf(out, "%-14s %v\n", name, err)
					continue
				}
				marker := " "
				if name == file.Default {
					marker = "*"
				}
				_, _ = fmt.Fprintf(out, "%s%-13s %-30s %s\n",
					marker, name,
					fmt.Sprintf("%s@%s:%d", cfg.User, cfg.Host, cfg.Port),
					proxy.DockerHost(cfg.EndpointFor(proxy.DefaultEndpoint)))
			}
			_, _ = fmt.Fprintln(out, "\n* default")
			return nil
		},
	}
}
