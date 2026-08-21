package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/backend/qemu"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Timeouts for the start stages (see stageTimeout for the TCG scaling).
const (
	sshTimeout       = 5 * time.Minute
	provisionTimeout = 15 * time.Minute
	provisionPoll    = 5 * time.Second
	// guestSocketTimeout bounds the wait for the podman socket after the
	// ready marker; podman_service starts as part of provisioning, so it
	// is normally there already.
	guestSocketTimeout = 30 * time.Second
	// tcgTimeoutScale stretches the stage timeouts under QEMU TCG (pure
	// emulation, as the guest-image CI job runs on amd64): first boot plus
	// the package install take hours there rather than minutes.
	tcgTimeoutScale = 8
)

// stageTimeout returns d, or d scaled by tcgTimeoutScale when the QEMU
// backend runs without hardware acceleration. HVF keeps the defaults.
func stageTimeout(d time.Duration) time.Duration {
	if qemu.Accel() == qemu.AccelTCG {
		return d * tcgTimeoutScale
	}
	return d
}

func newStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start [name]",
		Short: "Boot a machine and connect podman to it",
		Long: "Boot a machine in stages: network provider, hypervisor, SSH, first-boot\n" +
			"provisioning, podman connection, port forwarder. Starting a running machine\n" +
			"re-checks the ssh, provision, connect and forwarder stages (so an interrupted start can be\n" +
			"finished); a broken one (half of it running) is stopped and started again.\n\n" +
			"On failure the error names the stage and the log to read: qemu.log and\n" +
			"console.log (hypervisor), gvproxy.log and forward.log (networking),\n" +
			"forwarder.log (port publishing), or /var/log/jm-provision.log in the guest.",
		Example: `  jm start
  jm start dev
  jm -q start && jpodman run --rm --os=linux docker.io/alpine echo hi
  jm start --set-default   # make plain 'podman' use this machine`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd.Context(), args)
		},
	}
	cmd.Flags().BoolVar(&setDefaultConnection, "set-default", false, "make this machine the default podman connection (otherwise use jpodman / jm podman)")
	return cmd
}

func runStart(ctx context.Context, args []string) error {
	return startMachine(ctx, args, startOpts{})
}

// startOpts vary "jm start" for autostart (see autostart.go).
type startOpts struct {
	// waitLock queues behind another jm command operating on the machine
	// instead of failing: two wrappers racing to boot the same stopped
	// machine must both end up with a running one (ADR 0005: start is
	// idempotent and resumable).
	waitLock bool
	// skipIfReady returns as soon as the lock shows the machine already
	// booted and its engine answering — which is what the wrapper that
	// waited behind the one that did the work finds.
	skipIfReady bool
}

// startMachine is "jm start", with the variations autostart needs.
func startMachine(ctx context.Context, args []string, opts startOpts) error {
	m, err := loadMachine(args)
	if err != nil {
		return err
	}
	b, p, err := components(m)
	if err != nil {
		return err
	}
	unlock, err := lockMaybeWait(ctx, m.Name, opts.waitLock)
	if err != nil {
		return err
	}
	defer unlock()

	st, err := stateOf(m, b, p)
	if err != nil {
		return err
	}
	if opts.skipIfReady && st == backend.Running && engineReachable(m) {
		return nil
	}
	// $JM_PUBLISH_ADDR is an override read here, once, and written onto the
	// record: the forwarder runs detached, so the address it binds must be
	// on the machine rather than in this shell's environment (ADR 0004).
	//
	// It is read under the lock and only on the path that actually starts
	// the machine: the record is shared state, and a jpodman shell that
	// exports the variable must not rewrite the record of a machine that is
	// already running with another address — which this invocation would
	// not restart, so the running forwarder would go on binding the old one
	// while "jm inspect" showed the new.
	if err := applyPublishAddrEnv(m); err != nil {
		return err
	}
	if st == backend.Broken {
		// Half of the machine is up or stale; converge both halves to
		// stopped before starting them in order (ADR 0005).
		if err := repairBroken(ctx, m, b, p, true); err != nil {
			return err
		}
	}

	// Stage: network. The provider comes up first: the hypervisor may
	// connect to it. On a running machine p.Start returns the current
	// attachment and endpoint without touching anything.
	if st != backend.Running {
		logf(stdout, "%s: starting %s networking", machine.StageNetwork, p.Name())
	}
	// The link size is fixed when the provider starts (gvproxy reads
	// $JM_MTU there), so record it: doctor and inspect must report the
	// machine as it runs, not as this shell would start it.
	if caps := p.Capabilities(); caps.MTU != m.MTU {
		m.MTU = caps.MTU
		if err := store().Save(m); err != nil {
			return err
		}
	}
	att, ep, err := p.Start(ctx, m)
	if err != nil {
		return machine.NewStageError(machine.StageNetwork, networkHint(m, p), err)
	}
	if ep.GuestIP != m.GuestIP {
		m.GuestIP = ep.GuestIP
		if err := store().Save(m); err != nil {
			return err
		}
	}

	// Stage: dns (host half). The resolver that answers the guest's queries
	// from the host's own resolver runs on the host and does not need the
	// guest, so it comes up with the network (ADR 0008); the guest is
	// pointed at it once sshd answers.
	resolverPort, err := startResolver(ctx, m, ep)
	if err != nil {
		return err
	}

	// Stage: backend. A running machine skips the boot but still goes
	// through the remaining stages, which are idempotent: an interrupted
	// first boot, a failed connect or a dead forward is finished here
	// rather than needing jm stop && jm start (ADR 0005: start is
	// resumable).
	if st == backend.Running {
		logf(stdout, "%s: %s is already running (ssh %s:%d); checking the remaining stages", machine.StageBackend, m.Name, ep.SSHHost, ep.SSHPort)
	} else {
		logf(stdout, "%s: booting %s (%d cpus, %d MiB, ssh on %s:%d)", machine.StageBackend, m.Name, m.CPUs, m.MemoryMiB, ep.SSHHost, ep.SSHPort)
		// A share whose host path has gone is dropped by the backend
		// rather than allowed to keep the machine from booting (ADR
		// 0007); say so once, here, where the user can see it.
		warnUnsupportedShares(m, b)
		warnMissingShares(m.Shares)
		if err := b.Start(ctx, m, att); err != nil {
			// Do not leave a provider running for a hypervisor that never
			// started; that would read as "broken" afterwards.
			_ = p.Stop(ctx, m)
			return machine.NewStageError(machine.StageBackend, consoleHint(m, b), err)
		}
	}

	// Stage: ssh.
	logf(stdout, "%s: waiting for sshd", machine.StageSSH)
	client, err := waitSSH(ctx, m, b, p, ep)
	if err != nil {
		return err
	}
	defer client.Close()
	// A disk grown by "jm set --disk" while stopped is handed to the guest
	// now that sshd answers.
	finishPendingGrow(ctx, m, client)
	// The guest keeps its own clock in step with the host's (see clock.go);
	// correct it once here as well, so a machine that was already running
	// through a host sleep, or a guest too old to carry the resync service,
	// is right before anything is built or run in it.
	syncGuestClock(ctx, m, client)

	// Stage: provision.
	logf(stdout, "%s: waiting for %s", machine.StageProvision, machine.GuestProvisionMarker)
	if err := waitProvisioned(ctx, m, b, p, ep, client); err != nil {
		return err
	}
	// The official pkgbase image may have installed a new kernel during
	// its first boot (firstboot_pkg_upgrade); provision.sh cancels the
	// reboot it requested so the script is not cut short, and start
	// performs it here, before podman is connected: the Linuxulator
	// modules on disk only load on the kernel they were built for.
	if err := rebootForNewKernel(ctx, m, b, p, ep, client); err != nil {
		return err
	}
	if err := waitGuestSocket(ctx, m, client); err != nil {
		return err
	}
	// A guest too old to carry the jm_shares service attaches the shared
	// devices and mounts nothing, which is silent everywhere else (ADR
	// 0007); say so here, as the clock check does for jm_rtcsync.
	warnMissingShareSupport(ctx, m, client)

	// Stage: dns (guest half): the guest and its containers get exactly one
	// nameserver, the host resolver started above.
	if err := configureGuestDNS(ctx, m, client, ep, resolverPort); err != nil {
		return err
	}

	// Stage: connect. Providers whose API socket needs guest sshd (the
	// gvproxy ssh forward) bring it up now that the guest is ready.
	if f, ok := p.(netprov.APIForwarder); ok && ep.APISocket != "" {
		logf(stdout, "%s: forwarding %s to the guest podman socket", machine.StageConnect, ep.APISocket)
		if err := f.StartAPIForward(ctx, m); err != nil {
			return machine.NewStageError(machine.StageConnect, networkHint(m, p), err)
		}
	}
	logf(stdout, "%s: configuring podman connection %q", machine.StageConnect, m.Name)
	forgetHostKey(m)
	podmanConnectionRemove(ctx, m)
	if err := podmanConnectionAdd(ctx, m, ep); err != nil {
		return machine.NewStageError(machine.StageConnect, "is podman installed? brew install podman; then re-run 'jm start"+nameHint(m.Name)+"'", err)
	}
	if ep.APISocket != "" {
		logf(stdout, "%s: podman connection %q -> %s", machine.StageConnect, m.SocketConnectionName(), machine.SocketURI(ep.APISocket))
	}
	if !m.Provisioned {
		m.Provisioned = true
		if err := store().Save(m); err != nil {
			return err
		}
	}

	// Stage: forwarder. Publishes container ports on the host by
	// reconciling the provider's mapping table against the guest's
	// containers (ADR 0004); detached, so it outlives this command.
	if err := startForwarder(m, p, ep); err != nil {
		return err
	}
	logf(stdout, "ready: try 'jpodman run --rm --os=linux docker.io/alpine echo hi'")
	return nil
}

// componentDied reports, as a StageError for stage, whether the hypervisor
// or the network provider stopped running while "during" was going on; nil
// while both are up. It makes the wait loops fail fast with the real cause
// instead of timing out on sshd.
func componentDied(m *machine.Machine, b backend.Backend, p netprov.Provider, stage machine.Stage, during string) error {
	if st, err := b.State(m); err == nil && st != backend.Running {
		return machine.NewStageError(stage, consoleHint(m, b), errors.New("hypervisor exited "+during))
	}
	if st, err := p.State(m); err == nil && st != backend.Running {
		return machine.NewStageError(stage, networkHint(m, p), errors.New(p.Name()+" networking exited "+during))
	}
	return nil
}

// waitSSH polls sshd, printing a dot per attempt and bailing out early if
// the hypervisor or the network provider dies.
func waitSSH(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, ep netprov.Endpoint) (*sshx.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, stageTimeout(sshTimeout))
	defer cancel()
	var dead error
	client, err := sshx.WaitReady(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m), func(attempt int) {
		dot()
		if dead == nil {
			if dead = componentDied(m, b, p, machine.StageSSH, "while waiting for ssh"); dead != nil {
				cancel()
			}
		}
	})
	endDots()
	if dead != nil {
		return nil, dead
	}
	if err != nil {
		return nil, machine.NewStageError(machine.StageSSH, consoleHint(m, b), err)
	}
	return client, nil
}

// waitProvisioned waits for the ready marker (ADR 0003). On first boot the
// provisioning script installs packages, which takes minutes.
func waitProvisioned(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, ep netprov.Endpoint, client *sshx.Client) error {
	ctx, cancel := context.WithTimeout(ctx, stageTimeout(provisionTimeout))
	defer cancel()
	hint := fmt.Sprintf("log: jm ssh%s tail -f %s", nameHint(m.Name), machine.GuestProvisionLog)

	// The failure marker wins over a stale ready marker (a re-provisioned
	// disk whose script aborted), on the first probe as in the loop.
	if failed, ferr := client.FileExists(ctx, machine.GuestProvisionFailed); ferr == nil && failed {
		return machine.NewStageError(machine.StageProvision, hint, errors.New("provisioning script failed in the guest"))
	}
	ok, err := client.FileExists(ctx, machine.GuestProvisionMarker)
	switch {
	case err == nil && ok:
		return nil
	case err == nil:
		// Marker genuinely absent: the provisioning script is running.
		logf(stdout, "%s: first boot, installing packages (a few minutes; %s)", machine.StageProvision, hint)
	default:
		// Transport error: sshd may be restarting; say so rather than
		// announcing a first boot that may not be happening.
		logf(stdout, "%s: ssh connection dropped (%v); waiting for the guest", machine.StageProvision, err)
	}
	for {
		select {
		case <-ctx.Done():
			return machine.NewStageError(machine.StageProvision, hint, errors.New("timed out waiting for provisioning"))
		case <-time.After(provisionPoll):
		}
		dot()
		if dead := componentDied(m, b, p, machine.StageProvision, "during provisioning"); dead != nil {
			endDots()
			return dead
		}
		if failed, ferr := client.FileExists(ctx, machine.GuestProvisionFailed); ferr == nil && failed {
			endDots()
			return machine.NewStageError(machine.StageProvision, hint, errors.New("provisioning script failed in the guest"))
		}
		ok, err = client.FileExists(ctx, machine.GuestProvisionMarker)
		if err != nil {
			// The guest may restart sshd mid-provisioning: reconnect,
			// closing the dead connection before replacing it.
			if c, derr := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m)); derr == nil {
				_ = client.Close()
				*client = *c
			}
			continue
		}
		if ok {
			endDots()
			return nil
		}
	}
}

// kernelVersionCmd prints the on-disk kernel version and then the running
// one, one per line.
const kernelVersionCmd = "freebsd-version -k; freebsd-version -r"

// parseKernelVersions splits the output of kernelVersionCmd into the
// on-disk and running kernel versions. It returns ok=false when the output
// does not have both lines (freebsd-version missing, garbled output), in
// which case the caller must not reboot.
func parseKernelVersions(out string) (disk, running string, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return "", "", false
	}
	disk, running = strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	return disk, running, disk != "" && running != ""
}

// rebootForNewKernel reboots the guest once when the kernel on disk differs
// from the running one and waits for sshd to come back, replacing *client
// with a fresh connection. A guest that is already running its on-disk
// kernel (every warm start, non-pkgbase images) is left alone.
func rebootForNewKernel(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, ep netprov.Endpoint, client *sshx.Client) error {
	hint := fmt.Sprintf("check: jm ssh%s '%s'", nameHint(m.Name), kernelVersionCmd)
	out, _, err := client.Run(ctx, kernelVersionCmd)
	if err != nil {
		return machine.NewStageError(machine.StageProvision, hint, fmt.Errorf("reading the guest kernel version: %w", err))
	}
	disk, running, ok := parseKernelVersions(out)
	if !ok || disk == running {
		return nil
	}
	logf(stdout, "%s: kernel %s installed on first boot, running %s; rebooting the guest once", machine.StageProvision, disk, running)
	// The connection usually drops before shutdown returns: that is not
	// a failure. Bound it so a hung session cannot stall start.
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, _, _ = client.Run(sctx, "shutdown -r now")
	cancel()
	_ = client.Close()

	// Wait for sshd to go away, so the next wait cannot latch on to the
	// old instance still answering while the shutdown proceeds.
	dctx, dcancel := context.WithTimeout(ctx, time.Minute)
	defer dcancel()
	for {
		c, derr := sshx.Dial(dctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
		if derr != nil {
			break
		}
		_ = c.Close()
		select {
		case <-dctx.Done():
		case <-time.After(sshx.PollInterval):
			continue
		}
		break
	}

	logf(stdout, "%s: waiting for sshd after the reboot", machine.StageSSH)
	c, err := waitSSH(ctx, m, b, p, ep)
	if err != nil {
		return err
	}
	*client = *c
	out, _, err = client.Run(ctx, kernelVersionCmd)
	if err != nil {
		return machine.NewStageError(machine.StageProvision, hint, fmt.Errorf("reading the guest kernel version after the reboot: %w", err))
	}
	if disk, running, ok = parseKernelVersions(out); ok && disk != running {
		return machine.NewStageError(machine.StageProvision, hint,
			fmt.Errorf("guest still runs kernel %s after rebooting into %s", running, disk))
	}
	return nil
}

// waitGuestSocket checks that the guest podman API socket exists once the
// ready marker is there: a marker without the socket means provisioning
// did not really complete (packages missing, podman_service not running),
// which must surface here rather than as an opaque podman error later.
func waitGuestSocket(ctx context.Context, m *machine.Machine, client *sshx.Client) error {
	ctx, cancel := context.WithTimeout(ctx, guestSocketTimeout)
	defer cancel()
	hint := fmt.Sprintf("podman API socket %s missing in the guest; check: jm ssh%s 'service podman_service status; tail %s /var/log/podman_service.log'",
		machine.GuestPodmanSocket, nameHint(m.Name), machine.GuestProvisionLog)
	for {
		ok, err := client.SocketExists(ctx, machine.GuestPodmanSocket)
		if err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			if err == nil {
				err = fmt.Errorf("guest podman socket did not appear (see %s in the guest)", machine.GuestProvisionLog)
			}
			return machine.NewStageError(machine.StageProvision, hint, err)
		case <-time.After(sshx.PollInterval):
		}
	}
}
