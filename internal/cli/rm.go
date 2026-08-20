package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func newRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm [name]",
		Short: "Remove a machine and all its state",
		Long:  "Stop the machine if needed, forget its podman connection and host key, and delete its directory. Always converges to \"gone\".",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := machine.ResolveName(args)
			if err != nil {
				return err
			}
			s := store()
			if _, err := os.Stat(s.Dir(name)); err != nil {
				fmt.Fprintf(stdout, "%s does not exist; nothing to remove\n", name)
				return nil
			}
			unlock, err := lock(name)
			if err != nil {
				return err
			}
			defer unlock()

			ctx := cmd.Context()
			m, err := s.Load(name)
			if err != nil {
				// A half-initialised directory: nothing is running, just delete.
				fmt.Fprintf(stderr, "jm: %v; removing directory anyway\n", err)
			} else {
				if err := stopMachine(ctx, m, !force); err != nil {
					if !force {
						return fmt.Errorf("%w (use --force to remove anyway)", err)
					}
					fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
				}
				podmanConnectionRemove(ctx, m)
				forgetHostKey(m)
			}
			if err := s.Delete(name); err != nil {
				return err
			}
			logf(stdout, "removed %s", s.Dir(name))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "kill the hypervisor and ignore errors")
	return cmd
}
