package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

func newSSHCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh [name] [-- command...]",
		Short: "Open a shell or run a command in a machine",
		Long: "Open an interactive shell in the machine, or run a command. If the first\n" +
			"argument is not an existing machine name, every argument is the command and\n" +
			"the default machine is used.",
		Example: `  jm ssh
  jm ssh dev
  jm ssh -- uname -a
  jm ssh dev tail -f /var/log/jm-provision.log`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest := splitSSHArgs(args, store().Exists)
			var nameArgs []string
			if name != "" {
				nameArgs = []string{name}
			}
			m, err := loadMachine(nameArgs)
			if err != nil {
				return err
			}
			st, err := currentState(m)
			if err != nil {
				return err
			}
			if st != backend.Running {
				return withHint(fmt.Errorf("%s is not running", m.Name), fmt.Sprintf("run 'jm start%s'", nameHint(m.Name)))
			}
			ep, err := endpointOf(m)
			if err != nil {
				return err
			}
			return sshx.Interactive(ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m), rest)
		},
	}
	// Flags after the machine name belong to the remote command.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// splitSSHArgs decides whether args[0] names a machine. Anything else is
// the remote command, run on the default machine (name == "", resolved by
// loadMachine).
func splitSSHArgs(args []string, exists func(string) bool) (name string, rest []string) {
	if len(args) > 0 && machine.ValidateCLIName(args[0]) == nil && exists(args[0]) {
		return args[0], args[1:]
	}
	return "", args
}
