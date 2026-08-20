package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

func newStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop [name]",
		Short: "Shut a machine down",
		Long:  "Ask the guest to power off, stop the hypervisor, then the network provider. Stopping a stopped machine is a no-op.",
		Args:  cobra.MaximumNArgs(1),
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
			return stopMachine(cmd.Context(), m, !force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "terminate the hypervisor without a guest shutdown")
	return cmd
}

// stopMachine converges a machine to stopped: guest poweroff, hypervisor,
// then network provider (the reverse of start). The caller holds the lock.
func stopMachine(ctx context.Context, m *machine.Machine, graceful bool) error {
	b, p, err := components(m)
	if err != nil {
		return err
	}
	st, err := stateOf(m, b, p)
	if err != nil {
		return err
	}
	switch st {
	case backend.Stopped:
		fmt.Fprintf(stdout, "%s is not running\n", m.Name)
		return nil
	case backend.Broken:
		return repairBroken(ctx, m, b, p, graceful)
	}
	// The forwarder goes first, while the provider can still unexpose
	// the mappings it owns.
	stopForwarder(ctx, m, p)
	if graceful {
		logf(stdout, "stopping %s", m.Name)
		guestPoweroff(ctx, m)
	} else {
		logf(stdout, "killing %s", m.Name)
	}
	if err := b.Stop(ctx, m, graceful); err != nil {
		return err
	}
	if err := p.Stop(ctx, m); err != nil {
		return err
	}
	logf(stdout, "stopped")
	return nil
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
