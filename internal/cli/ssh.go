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
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, rest := splitSSHArgs(args, store().Exists)
			m, err := loadMachine([]string{name})
			if err != nil {
				return err
			}
			st, err := currentState(m)
			if err != nil {
				return err
			}
			if st != backend.Running {
				return fmt.Errorf("%s is not running (run 'jm start%s')", m.Name, nameHint(m.Name))
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
// the remote command, run on the default machine.
func splitSSHArgs(args []string, exists func(string) bool) (name string, rest []string) {
	if len(args) > 0 && machine.ValidateCLIName(args[0]) == nil && exists(args[0]) {
		return args[0], args[1:]
	}
	return machine.DefaultName, args
}
