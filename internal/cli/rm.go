package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

func newRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm [name]",
		Short: "Remove a machine and all its state",
		Long:  "Stop the machine if needed, forget its podman connection and host key, and delete its directory. Always converges to \"gone\".",
		Example: `  jm rm
  jm rm --force dev`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// rm takes the literal name when given and never guesses among
			// several machines; a half-initialised directory without a
			// record still resolves, so check the directory, not the store.
			name := machine.DefaultName
			var err error
			if len(args) > 0 {
				name, err = machine.ResolveName(args)
				if err != nil {
					return usage(err)
				}
			} else if _, statErr := os.Stat(store().Dir(name)); statErr != nil {
				if name, err = resolveName(nil); err != nil {
					return err
				}
			}
			activeMachine = name
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
				// A corrupt or missing record: a hypervisor may still be
				// running from this directory, so let the host's default
				// backend converge it before deleting.
				fmt.Fprintf(stderr, "jm: %v; removing directory anyway\n", err)
				// On a platform without a backend the name is empty and
				// backendFor fails below; the directory still goes.
				backendName, _ := backend.DefaultForHost()
				m = &machine.Machine{Name: name, Backend: backendName, Network: netprov.DefaultForHost(), Dir: s.Dir(name)}
				if p, perr := providerFor(m); perr == nil {
					stopForwarder(ctx, m, p)
				}
				if b, berr := backendFor(m); berr == nil {
					if serr := b.Stop(ctx, m, false); serr != nil {
						fmt.Fprintf(stderr, "jm: %v; continuing\n", serr)
					}
				}
				if p, perr := providerFor(m); perr == nil {
					if serr := p.Stop(ctx, m); serr != nil {
						fmt.Fprintf(stderr, "jm: %v; continuing\n", serr)
					}
				}
			} else {
				if err := stopMachine(ctx, m, !force); err != nil {
					if !force {
						return withHint(err, "use 'jm rm --force"+nameHint(name)+"' to remove anyway")
					}
					fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
					// stopMachine may have failed before reaching the
					// forwarder; never leave one behind.
					if p, perr := providerFor(m); perr == nil {
						stopForwarder(ctx, m, p)
					}
				}
				podmanConnectionRemove(ctx, m)
				forgetHostKey(m)
			}
			// Backends and providers may keep sockets outside the directory
			// (ADR 0005 addendum); remove them so rm really converges to
			// "gone".
			var cleaners []any
			if b, berr := backendFor(m); berr == nil {
				cleaners = append(cleaners, b)
			}
			if p, perr := providerFor(m); perr == nil {
				cleaners = append(cleaners, p)
			}
			for _, c := range cleaners {
				if c, ok := c.(backend.Cleaner); ok {
					if cerr := c.Cleanup(m); cerr != nil {
						fmt.Fprintf(stderr, "jm: %v; continuing\n", cerr)
					}
				}
			}
			if err := s.Delete(name); err != nil {
				return err
			}
			logf(stdout, "done: removed %s", s.Dir(name))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "kill the hypervisor and ignore errors")
	return cmd
}
