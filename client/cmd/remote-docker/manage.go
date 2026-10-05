package main

// Account management under `remote` (ADR 0053): tokens, accounts and keys,
// each one request to the workspace, which decides who may do what.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/lhns/remote-docker/client/internal/config"
	"github.com/lhns/remote-docker/client/internal/session"
	"github.com/lhns/remote-docker/core/enrol"
)

const timeFormat = "2006-01-02 15:04 UTC"

// manage sends one request as the resolved workspace's account, printing
// whatever notices come back. The host key is the one the workspace offered.
func manage(cmd *cobra.Command, req enrol.Request) (enrol.Reply, config.Config, ssh.PublicKey, error) {
	cfg, err := resolve(nil)
	if err != nil {
		return enrol.Reply{}, cfg, nil, err
	}
	if err := requireHost(cfg); err != nil {
		return enrol.Reply{}, cfg, nil, err
	}
	reply, hostKey, err := session.Manage(cmd.Context(), cfg, req)
	if errors.Is(err, session.ErrPredatesManagement) {
		return reply, cfg, nil, fmt.Errorf("%w\n  fix: ask its operator to upgrade it", err)
	}
	for _, n := range reply.Notices {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), n.Error())
	}
	return reply, cfg, hostKey, err
}

func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, list and remove enrolment tokens",
		Long: `An enrolment token lets another device enrol its own key: it runs the command
"token create" prints. Without --account the token is for your own account,
which is how you add a second machine; only an admin may name another account,
or create a token that makes a new one.`,
		Args: onlySubcommands,
		RunE: helpWhenBare,
	}
	cmd.AddCommand(newTokenCreateCommand(), newTokenListCommand(), newTokenRemoveCommand())
	return cmd
}

func newTokenCreateCommand() *cobra.Command {
	var account, note string
	var unbound bool
	var expires time.Duration

	cmd := &cobra.Command{
		Use:   "create [--account <name> | --unbound]",
		Short: "Create a single-use token and print the command that redeems it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if account != "" && unbound {
				return errors.New("--account and --unbound cannot both be given\n  fix: leave out one of them")
			}
			reply, cfg, hostKey, err := manage(cmd, enrol.Request{
				Op: enrol.OpTokenCreate, Account: account, Unbound: unbound, Expires: expires, Note: note,
			})
			if err != nil {
				return err
			}
			t := reply.Token
			if t == nil || hostKey == nil {
				return errors.New("the workspace answered with no token")
			}
			// The address this machine reaches the workspace at.
			transport, err := cfg.Transport()
			if err != nil {
				return err
			}
			invite := enrol.Invite{URL: transport.String(), HostKey: ssh.FingerprintSHA256(hostKey), Account: t.Account, Token: t.Token}
			name := cfg.Name
			if name == "" {
				name = "workspace"
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "docker remote create %s --token %s\n", name, invite)
			who := "for account " + t.Account
			if t.Account == "" {
				who = "for a new account the device names with --user"
			}
			_, _ = fmt.Fprintf(out, "single use, %s, expires %s\n", who, t.Expires.UTC().Format(timeFormat))
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "the account the key joins (default your own)")
	cmd.Flags().BoolVar(&unbound, "unbound", false, "create a new account the device names (admins only)")
	cmd.Flags().DurationVar(&expires, "expires", 24*time.Hour, "how long the token lives, at most 168h")
	cmd.Flags().StringVar(&note, "note", "", "a note shown by `token ls`")
	return cmd
}

func newTokenListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the tokens not yet redeemed: your own, or every one for an admin",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpTokenList})
			if err != nil {
				return err
			}
			if len(reply.Tokens) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no tokens")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tACCOUNT\tEXPIRES\tCREATOR\tNOTE")
			for _, t := range reply.Tokens {
				account := t.Account
				if account == "" {
					account = "(new)"
				}
				expires := t.Expires.UTC().Format(timeFormat)
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
			if _, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpTokenRemove, ID: args[0]}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed token %s\n", args[0])
			return nil
		},
	}
}

func newUserCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "user",
		Aliases: []string{"users"},
		Short:   "List and remove the workspace's accounts (admins only)",
		Args:    onlySubcommands,
		RunE:    helpWhenBare,
	}
	cmd.AddCommand(newUserListCommand(), newUserRemoveCommand())
	return cmd
}

func newUserListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List every account, its keys and whether it is an admin",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpUserList})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ACCOUNT\tUID\tKEYS\tADMIN\tSTATE")
			for _, u := range reply.Users {
				uid, admin := "-", ""
				if u.UID != 0 {
					uid = fmt.Sprint(u.UID)
				}
				if u.Admin {
					admin = "yes"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", u.Name, uid, keyCounts(u.Sources), admin, u.State)
			}
			return w.Flush()
		},
	}
}

// keyCounts is an account's keys per kind of directory: "1 enrolled, 2 operator".
func keyCounts(sources []enrol.KeySource) string {
	var enrolled, operator int
	for _, src := range sources {
		if src.Operator {
			operator += src.Keys
		} else {
			enrolled += src.Keys
		}
	}
	var parts []string
	if enrolled > 0 {
		parts = append(parts, fmt.Sprintf("%d enrolled", enrolled))
	}
	if operator > 0 {
		parts = append(parts, fmt.Sprintf("%d operator", operator))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

func newUserRemoveCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <account>",
		Aliases: []string{"remove"},
		Short:   "Revoke an account's keys and remove its daemon, keeping its data",
		Long: `Revokes every key the workspace enrolled for the account, closes its
connections and removes its daemon container. Its images, containers' storage,
home directory, uid and ports are kept, so a new token for the same name brings
it back as it was. Refused while its daemon runs containers, unless -f.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpUserRemove, Account: args[0], Force: force}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s, keeping their uid and storage\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove it even while its containers run")
	return cmd
}

func newKeyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "key",
		Aliases: []string{"keys"},
		Short:   "List, add and remove your account's keys",
		Args:    onlySubcommands,
		RunE:    helpWhenBare,
	}
	cmd.AddCommand(newKeyListCommand(), newKeyAddCommand(), newKeyRemoveCommand())
	return cmd
}

func newKeyListCommand() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List an account's keys and the directory each is in",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reply, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpKeyList, Account: account})
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "FINGERPRINT\tCOMMENT\tDIRECTORY\t")
			for _, k := range reply.Keys {
				this := ""
				if k.Current {
					this = "this machine"
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.Fingerprint, k.Comment, k.Dir, this)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "whose keys (default your own)")
	return cmd
}

func newKeyAddCommand() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "add <file|->",
		Short: "Enrol a public key, read from a .pub file or from stdin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			line, key, err := readPublicKey(cmd.InOrStdin(), args[0])
			if err != nil {
				return err
			}
			reply, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpKeyAdd, Account: account, Key: line})
			if err != nil {
				return err
			}
			if len(reply.Notices) == 0 {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added %s\n", ssh.FingerprintSHA256(key))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "whose key (default your own)")
	return cmd
}

// readPublicKey reads the first key in a .pub file, or in stdin for "-".
func readPublicKey(stdin io.Reader, path string) (string, ssh.PublicKey, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(io.LimitReader(stdin, enrol.MaxMessage))
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return "", nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
			return line, key, nil
		}
	}
	return "", nil, fmt.Errorf("%s holds no public key\n  fix: pass a .pub file, such as the one `%s` prints", path, ourCommand("enroll"))
}

func newKeyRemoveCommand() *cobra.Command {
	var account string
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <SHA256:fingerprint>",
		Aliases: []string{"remove"},
		Short:   "Remove a key, by the fingerprint `key ls` prints",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, _, _, err := manage(cmd, enrol.Request{Op: enrol.OpKeyRemove, Account: account, Fingerprint: args[0], Force: force}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "whose key (default your own)")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "remove the key this machine uses, or your last one")
	return cmd
}
