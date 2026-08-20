package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Timeouts for the start stages.
const (
	sshTimeout       = 5 * time.Minute
	provisionTimeout = 15 * time.Minute
	provisionPoll    = 5 * time.Second
)

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start [name]",
		Short: "Boot a machine and connect podman to it",
		Long: "Boot a machine in stages: hypervisor, SSH, first-boot provisioning, podman\n" +
			"connection. Starting a running machine is a no-op.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd.Context(), args)
		},
	}
}

func runStart(ctx context.Context, args []string) error {
	m, err := loadMachine(args)
	if err != nil {
		return err
	}
	b, err := backendFor(m)
	if err != nil {
		return err
	}
	unlock, err := lock(m.Name)
	if err != nil {
		return err
	}
	defer unlock()

	st, err := b.State(m)
	if err != nil {
		return err
	}
	if st == backend.Running {
		fmt.Fprintf(stdout, "%s is already running (ssh %s)\n", m.Name, m.SSHEndpoint())
		return nil
	}

	// Stage: backend.
	logf(stdout, "%s: booting %s (%d cpus, %d MiB, ssh on %s)", machine.StageBackend, m.Name, m.CPUs, m.MemoryMiB, m.SSHEndpoint())
	net := backend.NetAttachment{Kind: "user", HostFwdSSH: m.SSHPort, MAC: m.MAC}
	if err := b.Start(ctx, m, net); err != nil {
		return machine.NewStageError(machine.StageBackend, consoleHint(m, b), err)
	}

	// Stage: ssh.
	logf(stdout, "%s: waiting for sshd", machine.StageSSH)
	client, err := waitSSH(ctx, m, b)
	if err != nil {
		return err
	}
	defer client.Close()

	// Stage: provision.
	logf(stdout, "%s: waiting for %s", machine.StageProvision, machine.GuestProvisionMarker)
	if err := waitProvisioned(ctx, m, b, client); err != nil {
		return err
	}

	// Stage: connect.
	logf(stdout, "%s: configuring podman connection %q", machine.StageConnect, m.Name)
	forgetHostKey(m)
	podmanConnectionRemove(ctx, m)
	if err := podmanConnectionAdd(ctx, m); err != nil {
		return machine.NewStageError(machine.StageConnect, "is podman installed? brew install podman", err)
	}
	if !m.Provisioned {
		m.Provisioned = true
		if err := store().Save(m); err != nil {
			return err
		}
	}
	logf(stdout, "ready. Try: podman run --rm --os=linux docker.io/alpine echo hi")
	return nil
}

// waitSSH polls sshd, printing a dot per attempt and bailing out early if
// the hypervisor dies.
func waitSSH(ctx context.Context, m *machine.Machine, b backend.Backend) (*sshx.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	var dead error
	client, err := sshx.WaitReady(ctx, machine.SSHHost, m.SSHPort, m.SSHUser, sshKey(m), func(attempt int) {
		fmt.Fprint(stdout, ".")
		if st, serr := b.State(m); serr == nil && st != backend.Running && dead == nil {
			dead = fmt.Errorf("hypervisor exited while waiting for ssh")
			cancel()
		}
	})
	fmt.Fprintln(stdout)
	if dead != nil {
		return nil, machine.NewStageError(machine.StageSSH, consoleHint(m, b), dead)
	}
	if err != nil {
		return nil, machine.NewStageError(machine.StageSSH, consoleHint(m, b), err)
	}
	return client, nil
}

// waitProvisioned waits for the ready marker (ADR 0003). On first boot the
// provisioning script installs packages, which takes minutes.
func waitProvisioned(ctx context.Context, m *machine.Machine, b backend.Backend, client *sshx.Client) error {
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	hint := fmt.Sprintf("log: jm ssh%s tail -f %s", nameHint(m.Name), machine.GuestProvisionLog)

	ok, err := client.FileExists(ctx, machine.GuestProvisionMarker)
	if err == nil && ok {
		return nil
	}
	logf(stdout, "first boot: installing packages (a few minutes; %s)", hint)
	for {
		select {
		case <-ctx.Done():
			return machine.NewStageError(machine.StageProvision, hint, errors.New("timed out waiting for provisioning"))
		case <-time.After(provisionPoll):
		}
		fmt.Fprint(stdout, ".")
		if st, serr := b.State(m); serr == nil && st != backend.Running {
			fmt.Fprintln(stdout)
			return machine.NewStageError(machine.StageProvision, consoleHint(m, b), errors.New("hypervisor exited during provisioning"))
		}
		ok, err = client.FileExists(ctx, machine.GuestProvisionMarker)
		if err != nil {
			// The guest may restart sshd mid-provisioning: reconnect.
			if c, derr := sshx.Dial(ctx, machine.SSHHost, m.SSHPort, m.SSHUser, sshKey(m)); derr == nil {
				*client = *c
			}
			continue
		}
		if ok {
			fmt.Fprintln(stdout)
			return nil
		}
	}
}
