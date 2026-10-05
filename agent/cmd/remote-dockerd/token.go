package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/agent/internal/sshd"
	"github.com/lhns/remote-docker/core-agent/accounts"
	"github.com/lhns/remote-docker/core-agent/tokens"
	"github.com/lhns/remote-docker/core/enrol"
	"github.com/lhns/remote-docker/core/workspace"
)

const (
	// envTokensDir holds the enrolment tokens (ADR 0051).
	envTokensDir = "WORKSPACE_TOKENS_DIR"

	// envPublicURL is the address an invite gives a client: what it can
	// reach, which behind a proxy is not what the agent listens on.
	envPublicURL = "WORKSPACE_PUBLIC_URL"
)

func tokensDirFor(stateDir string) string {
	return envOr(envTokensDir, filepath.Join(stateDir, "tokens"))
}

func enrolledDirFor(stateDir string) string {
	return envOr(envEnrolledKeys, filepath.Join(stateDir, "enrolled_keys.d"))
}

func hostKeyDirFor(stateDir string) string {
	return envOr(envHostKeys, filepath.Join(stateDir, "host_keys"))
}

// The operator's way to enrol somebody: mint a token, hand over the line it
// prints, and the device enrols its own key. Run inside the workspace:
//
//	docker exec <workspace> remote-dockerd token create --account alice
//	kubectl exec deploy/<release> -- remote-dockerd token create --account alice
func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and remove single-use enrolment tokens",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newTokenCreateCommand(), newTokenListCommand(), newTokenRemoveCommand())
	return cmd
}

func newTokenCreateCommand() *cobra.Command {
	var account, note, publicURL string
	var unbound bool
	var expires time.Duration

	cmd := &cobra.Command{
		Use:   "create (--account <name> | --unbound)",
		Short: "Mint a token and print the command that redeems it",
		Long: `Mints a single-use token and prints the command a device runs to enrol its
own key with it. With --account the key joins that account, which is created
if it does not exist; --unbound creates a new account under a name the device
chooses, never one that exists or ever existed.

The address in the invite is --url, else WORKSPACE_PUBLIC_URL: what the
device can reach, such as wss://ws.example/ behind an ingress.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (account == "") == !unbound {
				return errors.New("name the account with --account, or pass --unbound\n  fix: `remote-dockerd token create --account alice`")
			}
			if account != "" {
				name, err := workspace.AccountName(account)
				if err != nil {
					return err
				}
				account = name
			}
			if publicURL == "" {
				publicURL = os.Getenv(envPublicURL)
			}
			if publicURL == "" {
				return fmt.Errorf("the invite needs the address devices reach this workspace at\n  fix: pass --url wss://<host>/, or set %s", envPublicURL)
			}

			stateDir := envOr(envStateDir, defaultStateDir)
			if dir := enrolledDirFor(stateDir); (&accounts.Store{EnrolledDir: dir}).CheckWritable() != nil {
				return sshd.CannotStore(dir)
			}
			// Generated here if the agent has never served, which is the key
			// it will then serve with.
			hostKeyDir := hostKeyDirFor(stateDir)
			if err := os.MkdirAll(hostKeyDir, 0o755); err != nil {
				return err
			}
			hostKeys, err := loadHostKeys(hostKeyDir)
			if err != nil {
				return err
			}

			store := &tokens.Store{Dir: tokensDirFor(stateDir)}
			token, t, err := store.Mint(account, "operator", note, expires)
			if err != nil {
				return err
			}
			invite := enrol.Invite{
				URL:     publicURL,
				HostKey: ssh.FingerprintSHA256(hostKeys[0].PublicKey()),
				Account: account,
				Token:   token,
			}
			printInvite(cmd.OutOrStdout(), invite, t)
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "the account the key joins, created if need be")
	cmd.Flags().BoolVar(&unbound, "unbound", false, "create a new account the device names")
	cmd.Flags().DurationVar(&expires, "expires", tokens.DefaultTTL, "how long the token lives, at most 168h")
	cmd.Flags().StringVar(&note, "note", "", "a note shown by `token ls`")
	cmd.Flags().StringVar(&publicURL, "url", "", "the workspace address in the invite (default $"+envPublicURL+")")
	return cmd
}

// printInvite prints the command to run, then what the token is.
func printInvite(out io.Writer, invite enrol.Invite, t tokens.Token) {
	_, _ = fmt.Fprintf(out, "docker remote create %s --token %s\n", remoteName(invite.URL), invite)
	who := "for account " + t.Account
	if t.Account == "" {
		who = "for a new account the device names with --user"
	}
	_, _ = fmt.Fprintf(out, "single use, %s, expires %s\n", who, t.Expires.UTC().Format("2006-01-02 15:04 UTC"))
}

// remoteName suggests a name for the remote: the first label of its host.
func remoteName(address string) string {
	host := address
	if u, err := url.Parse(address); err == nil && u.Host != "" {
		host = u.Hostname()
	} else if h, _, err := net.SplitHostPort(address); err == nil {
		host = h
	}
	if net.ParseIP(host) != nil || host == "" {
		return "workspace"
	}
	label, _, _ := strings.Cut(host, ".")
	if name, err := workspace.AccountName(label); err == nil {
		return name
	}
	return "workspace"
}

func newTokenListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the tokens not yet redeemed",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := &tokens.Store{Dir: tokensDirFor(envOr(envStateDir, defaultStateDir))}
			list, err := store.List()
			if err != nil {
				return err
			}
			if len(list) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no tokens")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tACCOUNT\tEXPIRES\tCREATOR\tNOTE")
			for _, t := range list {
				account := t.Account
				if account == "" {
					account = "(new)"
				}
				expires := t.Expires.UTC().Format("2006-01-02 15:04 UTC")
				if !time.Now().Before(t.Expires) {
					expires = "expired"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, account, expires, t.Creator, t.Note)
			}
			return w.Flush()
		},
	}
}

func newTokenRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>",
		Aliases: []string{"remove", "revoke"},
		Short:   "Remove a token before it is redeemed",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store := &tokens.Store{Dir: tokensDirFor(envOr(envStateDir, defaultStateDir))}
			if err := store.Revoke(args[0]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed token %s\n", args[0])
			return nil
		},
	}
}
