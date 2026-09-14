package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

func newStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop [name]",
		Short: "Shut a machine down",
		Long: "Ask the guest to power off, stop the hypervisor, then the network provider. Stopping a stopped machine is a no-op.\n\n" +
			"A suspended machine is restored first and then shut down cleanly; --force\n" +
			"discards a suspended machine's saved state instead.",
		Example: `  jm stop
  jm stop --force dev`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			unlock, err := lock(m.Name)
			if err != nil {
				return err
			}
			defer unlock()
			return stopMachine(cmd.Context(), m, !force, false)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "terminate the hypervisor without a guest shutdown; discards a suspended machine's saved state")
	return cmd
}

// stopMachine converges a machine to stopped: guest poweroff, hypervisor,
// then network provider (the reverse of start). The caller holds the lock.
//
// A suspended machine is restored and then shut down cleanly when graceful is
// set; without it, or with discard (for "jm rm"), its saved state is
// discarded and nothing is restored (ADR 0009).
func stopMachine(ctx context.Context, m *machine.Machine, graceful, discard bool) error {
	// The sleeper goes first: it holds a suspended machine's endpoints, and
	// must not start a suspend or a wake of a machine being stopped.
	stopSleeper(ctx, m)
	b, p, err := components(m)
	if err != nil {
		return err
	}
	// An interrupted suspend or wake is resolved first (ADR 0009). A forced
	// stop goes on without it: the hypervisor is about to be killed, so there
	// is no guest to continue, and a valid saved state survives the repair.
	if _, err := recoverInterruptedTransition(ctx, m, b, p); err != nil {
		if graceful {
			return err
		}
		fmt.Fprintf(stderr, "jm: warning: %v; stopping it anyway\n", err)
	}
	// The forwarder goes first, whatever the machine's state: while the
	// provider is still up it can unexpose the mappings it owns, and a
	// forwarder left over from a dead machine is tidied away. The host
	// resolver goes with it: it exists only to answer this guest.
	stopForwarder(ctx, m, p)
	stopResolver(ctx, m)
	st, err := stateOf(m, b, p)
	if err != nil {
		return err
	}
	if st == backend.Broken {
		if err := repairBroken(ctx, m, b, p, graceful); err != nil {
			return withHint(err, consoleHint(m, b)+"; "+networkHint(m, p))
		}
		// A saved state survives the repair.
		if st, err = stateOf(m, b, p); err != nil || st != backend.Suspended {
			return err
		}
	}
	switch st {
	case backend.Stopped:
		fmt.Fprintf(stdout, "%s is not running\n", m.Name)
		return nil
	case backend.Suspended:
		done, err := stopSuspended(ctx, m, b, p, graceful && !discard)
		if done || err != nil {
			return err
		}
	}
	if graceful {
		logf(stdout, "%s: asking the guest to power off, then stopping %s", machine.StageBackend, b.Name())
		guestPoweroff(ctx, m)
	} else {
		logf(stdout, "%s: killing %s", machine.StageBackend, b.Name())
	}
	if err := b.Stop(ctx, m, graceful); err != nil {
		return machine.NewStageError(machine.StageBackend, consoleHint(m, b), err)
	}
	logf(stdout, "%s: stopping %s networking", machine.StageNetwork, p.Name())
	if err := p.Stop(ctx, m); err != nil {
		return machine.NewStageError(machine.StageNetwork, networkHint(m, p), err)
	}
	logf(stdout, "done: %s stopped", m.Name)
	return nil
}

// stopSuspended is stopMachine on a suspended machine. With restore, the
// guest is restored and done is false: the caller shuts the running guest
// down. Otherwise, or when the saved state cannot be restored, the state is
// discarded, the provider stopped, and done is true. A restore that fails
// for any other reason keeps the machine suspended.
func stopSuspended(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, restore bool) (done bool, err error) {
	s, ok := b.(backend.Suspender)
	if !ok {
		return true, fmt.Errorf("backend %q cannot handle a suspended machine", b.Name())
	}
	if restore {
		resumed, err := wakeMachine(ctx, m, b, p, wakeOpts{ForShutdown: true})
		if err != nil {
			if bs, serr := b.State(m); serr == nil && bs == backend.Running && !suspendInProgress(m) {
				// The saved state is gone and the guest runs: there is
				// nothing left to keep, so shut it down as a running one.
				fmt.Fprintf(stderr, "jm: warning: %v; shutting the restored guest down\n", err)
				return false, nil
			}
			return true, withHint(err, "'jm stop --force"+nameHint(m.Name)+"' discards the saved state")
		}
		if resumed {
			return false, nil
		}
	} else {
		logf(stdout, "%s: discarding the saved state of %s", machine.StageBackend, m.Name)
		if err := s.DiscardSuspend(m, "jm stop --force or jm rm"); err != nil {
			return true, machine.NewStageError(machine.StageBackend, consoleHint(m, b), err)
		}
	}
	logf(stdout, "%s: stopping %s networking", machine.StageNetwork, p.Name())
	if err := p.Stop(ctx, m); err != nil {
		return true, machine.NewStageError(machine.StageNetwork, networkHint(m, p), err)
	}
	logf(stdout, "done: %s stopped", m.Name)
	return true, nil
}

// guestPoweroff asks the guest to shut down over SSH, like the PoC. Failure
// is fine: the backend falls back to ACPI powerdown and then signals.
func guestPoweroff(ctx context.Context, m *machine.Machine) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ep, err := endpointOf(m)
	if err != nil {
		return
	}
	c, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		return
	}
	defer c.Close()
	_, _, _ = c.Run(ctx, "poweroff")
}
