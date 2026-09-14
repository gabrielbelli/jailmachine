package cli

import (
	"context"
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
			"the default machine is used. A suspended machine is woken first.",
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
			// A command too short for the idle probe to see is still use.
			bumpActivity(m)
			st, err := currentState(m)
			if err != nil {
				return err
			}
			switch {
			case suspendedOrTransition(m, st):
				// ssh's own connect timeout is shorter than a wake, so the
				// wake happens here, before ssh runs (ADR 0009).
				fmt.Fprintf(stderr, "waking jailmachine %q...\n", m.Name)
				ctx, cancel := context.WithTimeout(cmd.Context(), autostartLockWait)
				err := startQuietlyFn(ctx, m.Name, true)
				cancel()
				if err != nil {
					return err
				}
			case st != backend.Running:
				return withHint(fmt.Errorf("%s is not running", m.Name), fmt.Sprintf("run 'jm start%s'", nameHint(m.Name)))
			}
			ep, err := endpointOf(m)
			if err != nil {
				return err
			}
			return sshInteractive(ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m), rest)
		},
	}
	// Flags after the machine name belong to the remote command.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// sshInteractive is sshx.Interactive; a variable so tests do not run ssh.
var sshInteractive = sshx.Interactive

// splitSSHArgs decides whether args[0] names a machine. Anything else is
// the remote command, run on the default machine (name == "", resolved by
// loadMachine).
func splitSSHArgs(args []string, exists func(string) bool) (name string, rest []string) {
	if len(args) > 0 && machine.ValidateCLIName(args[0]) == nil && exists(args[0]) {
		return args[0], args[1:]
	}
	return "", args
}
