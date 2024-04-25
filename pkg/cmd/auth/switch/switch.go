package authswitch

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/pkg/cmd/auth/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

type gitConfigurator interface {
	SwitchLocalGitUsernameAndEmail(email, username string) error
	SwitchGlobalGitUsernameAndEmail(email, username string) error
}

type SwitchOptions struct {
	IO                    *iostreams.IOStreams
	Config                func() (config.Config, error)
	HttpClient            func() (*http.Client, error)
	Prompter              shared.Prompt
	Hostname              string
	Username              string
	SwitchGitLocalConfig  bool
	SwitchGitGlobalConfig bool
	gitConfigure          gitConfigurator
}

func NewCmdSwitch(f *cmdutil.Factory, runF func(*SwitchOptions) error) *cobra.Command {
	opts := SwitchOptions{
		IO:         f.IOStreams,
		Config:     f.Config,
		Prompter:   f.Prompter,
		HttpClient: f.HttpClient,
	}

	cmd := &cobra.Command{
		Use:   "switch",
		Args:  cobra.ExactArgs(0),
		Short: "Switch active GitHub account",
		Long: heredoc.Docf(`
			Switch the active account for a GitHub host.

			This command changes the authentication configuration that will
			be used when running commands targeting the specified GitHub host.

			If the specified host has two accounts, the active account will be switched
			automatically. If there are more than two accounts, disambiguation will be
			required either through the %[1]s--user%[1]s flag or an interactive prompt.

			For a list of authenticated accounts you can run %[1]sgh auth status%[1]s.
		`, "`"),
		Example: heredoc.Doc(`
			# Select what host and account to switch to via a prompt
			$ gh auth switch

			# Switch to a specific host and specific account
			$ gh auth switch --hostname enterprise.internal --user monalisa
		`),
		RunE: func(c *cobra.Command, args []string) error {
			if runF != nil {
				return runF(&opts)
			}
			opts.gitConfigure = &shared.GitCredentialFlow{
				Executable: f.Executable(),
				GitClient:  f.GitClient,
			}

			if runF != nil {
				return runF(&opts)
			}

			return switchRun(&opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Hostname, "hostname", "h", "", "The hostname of the GitHub instance to switch account for")
	cmd.Flags().StringVarP(&opts.Username, "user", "u", "", "The account to switch to")
	cmd.Flags().BoolVarP(&opts.SwitchGitLocalConfig, "git-local-config", "l", false, "Switch the local git configuration to the selected account")
	cmd.Flags().BoolVarP(&opts.SwitchGitGlobalConfig, "git-global-config", "g", false, "Switch the global git configuration to the selected account")

	return cmd
}

type hostUser struct {
	host   string
	user   string
	active bool
}

type candidates []hostUser

func switchRun(opts *SwitchOptions) error {
	hostname := opts.Hostname
	username := opts.Username

	cfg, err := opts.Config()
	if err != nil {
		return err
	}
	authCfg := cfg.Authentication()

	knownHosts := authCfg.Hosts()
	if len(knownHosts) == 0 {
		return fmt.Errorf("not logged in to any hosts")
	}

	if hostname != "" {
		if !slices.Contains(knownHosts, hostname) {
			return fmt.Errorf("not logged in to %s", hostname)
		}

		if username != "" {
			knownUsers := cfg.Authentication().UsersForHost(hostname)
			if !slices.Contains(knownUsers, username) {
				return fmt.Errorf("not logged in to %s account %s", hostname, username)
			}
		}
	}

	var candidates candidates

	for _, host := range knownHosts {
		if hostname != "" && host != hostname {
			continue
		}
		hostActiveUser, err := authCfg.ActiveUser(host)
		if err != nil {
			return err
		}
		knownUsers := cfg.Authentication().UsersForHost(host)
		for _, user := range knownUsers {
			if username != "" && user != username {
				continue
			}
			candidates = append(candidates, hostUser{host: host, user: user, active: user == hostActiveUser})
		}
	}

	if len(candidates) == 0 {
		return errors.New("no accounts matched that criteria")
	} else if len(candidates) == 1 {
		hostname = candidates[0].host
		username = candidates[0].user
	} else if len(candidates) == 2 &&
		candidates[0].host == candidates[1].host {
		// If there is a single host with two users, automatically switch to the
		// inactive user without prompting.
		hostname = candidates[0].host
		username = candidates[0].user
		if candidates[0].active {
			username = candidates[1].user
		}
	} else if !opts.IO.CanPrompt() {
		return errors.New("unable to determine which account to switch to, please specify `--hostname` and `--user`")
	} else {
		prompts := make([]string, len(candidates))
		for i, c := range candidates {
			prompt := fmt.Sprintf("%s (%s)", c.user, c.host)
			if c.active {
				prompt += " - active"
			}
			prompts[i] = prompt
		}
		selected, err := opts.Prompter.Select(
			"What account do you want to switch to?", "", prompts)
		if err != nil {
			return fmt.Errorf("could not prompt: %w", err)
		}
		hostname = candidates[selected].host
		username = candidates[selected].user
	}

	if src, writeable := shared.AuthTokenWriteable(authCfg, hostname); !writeable {
		fmt.Fprintf(opts.IO.ErrOut, "The value of the %s environment variable is being used for authentication.\n", src)
		fmt.Fprint(opts.IO.ErrOut, "To have GitHub CLI manage credentials instead, first clear the value from the environment.\n")
		return cmdutil.SilentError
	}

	cs := opts.IO.ColorScheme()

	if err := authCfg.SwitchUser(hostname, username); err != nil {
		fmt.Fprintf(opts.IO.ErrOut, "%s Failed to switch account for %s to %s\n",
			cs.FailureIcon(), hostname, cs.Bold(username))

		return err
	}

	if opts.SwitchGitLocalConfig {
		username, email, err := fetchCurrentLoginNameAndEmail(opts, hostname)
		if err != nil {
			return err
		}
		err = opts.gitConfigure.SwitchLocalGitUsernameAndEmail(email, username)
		if err != nil {
			return err
		}
	} else if opts.SwitchGitGlobalConfig {
		username, email, err := fetchCurrentLoginNameAndEmail(opts, hostname)
		if err != nil {
			return err
		}
		err = opts.gitConfigure.SwitchGlobalGitUsernameAndEmail(email, username)
		if err != nil {
			return err
		}
	}

	fmt.Fprintf(opts.IO.ErrOut, "%s Switched active account for %s to %s\n",
		cs.SuccessIcon(), hostname, cs.Bold(username))

	return nil
}

func fetchCurrentLoginNameAndEmail(opts *SwitchOptions, hostname string) (string, string, error) {
	httpClient, err := opts.HttpClient()
	if err != nil {
		return "", "", err
	}

	apiClient := api.NewClientFromHTTP(httpClient)

	user, email, err := api.CurrentLoginNameAndEmail(apiClient, hostname)
	if err != nil {
		return "", "", err
	}
	if email == "" {
		// default noreply github email
		email = fmt.Sprintf("%s@users.noreply.github.com", user)
	}
	return user, email, nil
}
