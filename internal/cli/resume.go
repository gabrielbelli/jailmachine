package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/resolver"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Wake (ADR 0009): a suspended machine is restored from its saved state and
// reconnected, in seconds rather than a cold boot's tens of seconds.
const (
	// wakeSSHTimeout and wakeSSHPoll bound the dial loop after the guest is
	// continued: sshd is already running in the restored guest, so it
	// answers within a few attempts.
	wakeSSHTimeout = 60 * time.Second
	wakeSSHPoll    = 25 * time.Millisecond
	// postResumeTimeout bounds the one guest script a wake runs.
	postResumeTimeout = 30 * time.Second
)

// wakeOpts vary a wake.
type wakeOpts struct {
	// ForShutdown restores the guest only as far as running (W0 to W5),
	// for "jm stop" to shut it down cleanly.
	ForShutdown bool
	// FromHelper is a wake run by a detached jm helper rather than a user's
	// command; the environment of the shell that started it is not read.
	FromHelper bool
	// By names what woke the machine for the sleeper's status ("wrapper",
	// "jm start"); empty lets the sleeper name the endpoint it held.
	By string
}

// wakeMachine restores a suspended machine (steps W0 to W10). The caller holds
// the lock and has run recovery. resumed is true when the guest runs from its
// saved state. resumed false with a nil error means the saved state could not
// be restored and was discarded: the machine is stopped and the caller boots
// it from disk. Any other failure before the guest runs keeps the saved state
// and leaves the machine suspended; a failure after that leaves it running,
// for a later "jm start" to finish (ADR 0005).
//
// Nothing is read from the environment on this path: the MTU is the one the
// record captured at the cold boot, the hypervisor's command line is the saved
// one, and the publish address is the record's.
func wakeMachine(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, opts wakeOpts) (bool, error) {
	s, ok := b.(backend.Suspender)
	if !ok {
		return false, fmt.Errorf("backend %q cannot restore a suspended machine", b.Name())
	}
	began := time.Now()
	status, err := s.SuspendStatus(m)
	if err != nil {
		return false, withHint(fmt.Errorf("reading the saved state of %s: %w", m.Name, err), "'jm stop --force"+nameHint(m.Name)+"' discards it")
	}
	if opts.ForShutdown {
		logf(stdout, "%s: restoring %s to shut it down", machine.StageBackend, m.Name)
	} else {
		logf(stdout, "%s: waking %s from its saved state", machine.StageBackend, m.Name)
	}

	// W1: a sleeper holding the SSH port lets go of it for this process.
	if ep, err := p.Endpoint(m); err == nil {
		if err := releaseSleeperTCP(ctx, m, ep); err != nil {
			return false, wakeTransient(m, err)
		}
	}

	// W2: the network, and the resolver beside it.
	att, ep, err := p.Start(ctx, m)
	if err != nil {
		_ = parkProvider(ctx, m, p)
		return false, wakeTransient(m, machine.NewStageError(machine.StageNetwork, networkHint(m, p), err))
	}
	var (
		wg           sync.WaitGroup
		resolverPort int
		resolverErr  error
	)
	if !opts.ForShutdown {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resolverPort, resolverErr = startResolver(ctx, m, ep)
		}()
	}

	// W3 to W5: the backend launches, loads and continues.
	rerr := s.Resume(ctx, m, att)
	wg.Wait()
	if rerr != nil {
		if errors.Is(rerr, backend.ErrResumeIncompatible) {
			return false, discardIncompatible(ctx, m, s, p, status, rerr, opts)
		}
		if bs, serr := b.State(m); serr == nil && bs == backend.Running && !suspendInProgress(m) {
			// The journal is gone, so the guest is the machine now and is
			// never killed; recovery continues it.
			return false, machine.NewStageError(machine.StageBackend, consoleHint(m, b),
				fmt.Errorf("%s was restored but did not continue: %w; 'jm start%s' finishes it", m.Name, rerr, nameHint(m.Name)))
		}
		_ = parkProvider(ctx, m, p)
		stopResolver(ctx, m)
		return false, wakeTransient(m, rerr)
	}
	if opts.ForShutdown {
		return true, nil
	}
	if resolverErr != nil {
		fmt.Fprintf(stderr, "jm: warning: %v; 'jm doctor' re-checks name resolution\n", resolverErr)
		resolverPort = 0
	}

	// W6: sshd is live in the restored guest.
	client, err := dialAwake(ctx, m, ep)
	if err != nil {
		return true, machine.NewStageError(machine.StageSSH, consoleHint(m, b), err)
	}
	defer client.Close()

	// W7: one script for everything the frozen guest missed.
	if err := postResume(ctx, m, client, ep, status.Meta, resolverPort); err != nil {
		return true, err
	}

	// W8: the engine socket.
	if f, ok := p.(netprov.APIForwarder); ok && ep.APISocket != "" {
		if err := f.StartAPIForward(ctx, m); err != nil {
			return true, machine.NewStageError(machine.StageConnect, networkHint(m, p), err)
		}
	}

	// W9: the sleeper hands its held connections over now, rather than on
	// its next check.
	by := opts.By
	if by == "" {
		by = "-"
	}
	tellSleeper(ctx, m, fmt.Sprintf("woke %s %d", by, time.Since(began).Milliseconds()))

	// W10: the forwarder re-exposes its mappings on the new provider, and a
	// machine woken without its sleeper gets one back.
	if err := startForwarder(m, p, ep); err != nil {
		return true, err
	}
	if err := startSleeper(ctx, m, b, p); err != nil {
		fmt.Fprintf(stderr, "jm: warning: %v; %s is awake, but nothing will suspend it or hold its endpoints\n", err, m.Name)
	}
	bumpActivity(m)
	logf(stdout, "done: woke %s in %.1f s", m.Name, time.Since(began).Seconds())
	return true, nil
}

// wakeTransient is the error for a wake that failed without touching the
// saved state.
func wakeTransient(m *machine.Machine, err error) error {
	return withHint(fmt.Errorf("could not wake %s (%w)", m.Name, err),
		"retry, or 'jm stop --force"+nameHint(m.Name)+"' discards the saved state")
}

// discardIncompatible discards a saved state the backend cannot restore, and
// says what was lost.
func discardIncompatible(ctx context.Context, m *machine.Machine, s backend.Suspender, p netprov.Provider, status backend.SuspendStatus, cause error, opts wakeOpts) error {
	desc := detail(cause, backend.ErrResumeIncompatible)
	if err := s.DiscardSuspend(m, desc); err != nil {
		return fmt.Errorf("discarding the saved state of %s, which cannot be restored (%s): %w", m.Name, desc, err)
	}
	if err := parkProvider(ctx, m, p); err != nil {
		fmt.Fprintf(stderr, "jm: warning: %v; continuing\n", err)
	}
	when := "it was saved"
	if !status.SavedAt.IsZero() {
		when = status.SavedAt.Local().Format(time.RFC1123)
	}
	if opts.ForShutdown {
		fmt.Fprintf(stderr, "jm: could not restore the suspended state of %s (%s); discarded it: processes in the guest were not preserved, the disk is intact as of %s\n", m.Name, desc, when)
		return nil
	}
	fmt.Fprintf(stderr, "jm: could not restore the suspended state of %s (%s); booting from disk: processes in the guest were not preserved, the disk is intact as of %s\n", m.Name, desc, when)
	return nil
}

// dialAwake dials the restored guest's sshd in a tight loop.
func dialAwake(ctx context.Context, m *machine.Machine, ep netprov.Endpoint) (*sshx.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, wakeSSHTimeout)
	defer cancel()
	for {
		c, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
		if err == nil {
			return c, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the restored guest's sshd did not answer: %w", err)
		case <-time.After(wakeSSHPoll):
		}
	}
}

// postResume is W7. Everything in it is a warning except a full DNS
// reconfiguration, which fails the way the dns stage of a start does.
func postResume(ctx context.Context, m *machine.Machine, client *sshx.Client, ep netprov.Endpoint, meta map[string]string, resolverPort int) error {
	ctx, cancel := context.WithTimeout(ctx, postResumeTimeout)
	defer cancel()
	arc := ""
	if meta["arc_mib"] != strconv.Itoa(m.ArcMiB) {
		arc = arcScript(m.ArcMiB)
	}
	// The resolver restarts on its last port, so the guest's forwarder
	// still points at it and only the host's search list may have moved.
	// A different port needs the full dns stage.
	fullDNS := resolverPort != 0 && meta["resolver_port"] != strconv.Itoa(resolverPort)
	dns := ""
	if resolverPort != 0 && !fullDNS {
		g := resolver.GuestConfig{
			UpstreamIP:   ep.HostAlias,
			UpstreamPort: resolverPort,
			Nameserver:   ep.GuestIP,
			HostAlias:    ep.HostAlias,
			Search:       resolver.SearchDomains(ctx),
		}
		if script, err := g.Script(); err == nil {
			dns = script
		} else {
			fmt.Fprintf(stderr, "jm: warning: %s: %v\n", m.Name, err)
		}
	}
	out, _, err := client.Run(ctx, postResumeScript(time.Now().Unix(), arc, dns))
	if err != nil {
		fmt.Fprintf(stderr, "jm: warning: %s: the post-wake guest script failed: %v; 'jm start%s' finishes it\n", m.Name, err, nameHint(m.Name))
	}
	for _, l := range strings.Split(out, "\n") {
		if w, ok := strings.CutPrefix(strings.TrimSpace(l), "warn: "); ok {
			fmt.Fprintf(stderr, "jm: warning: %s: %s\n", m.Name, w)
		}
	}
	if fullDNS {
		return configureGuestDNS(ctx, m, client, ep, resolverPort)
	}
	return nil
}

// postResumeScript is the guest program a wake runs once sshd answers. The
// guest clock was frozen with the guest, so it is stepped to epoch (the host
// time just before sending) every time. Shares a suspend unmounted are
// mounted again; arc and dns, when not empty, are the ARC cap program and the
// guest DNS program, run in their own shells. Each step that fails prints a
// "warn: " line; the program itself always exits 0.
func postResumeScript(epoch int64, arc, dns string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "date -u -f %%s %d >/dev/null || echo 'warn: stepping the guest clock failed'\n", epoch)
	b.WriteString(jmRemountFn)
	b.WriteString("if [ -f " + machine.GuestSuspendMounts + " ]; then\n")
	b.WriteString("  if jm_remount; then rm -f " + machine.GuestSuspendMounts + "; else echo 'warn: shares unmounted by the suspend are not all back (retried on the next start)'; fi\n")
	b.WriteString("fi\n")
	if arc != "" {
		b.WriteString("/bin/sh -c " + shellQuote(arc) + " >/dev/null 2>&1 || echo 'warn: the ZFS ARC cap was not applied (retried on the next start)'\n")
	}
	if dns != "" {
		b.WriteString("/bin/sh -c " + shellQuote(dns) + " >/dev/null 2>&1 || echo 'warn: the guest DNS search list was not updated'\n")
	}
	b.WriteString("test -S " + machine.GuestPodmanSocket + " || echo 'warn: the guest podman socket is missing'\n")
	b.WriteString("exit 0\n")
	return b.String()
}

// bumpActivity marks host use of m, for the idle monitor.
func bumpActivity(m *machine.Machine) {
	if m.Dir == "" {
		return
	}
	path := filepath.Join(m.Dir, machine.ActivityFile)
	now := time.Now()
	if err := os.Chtimes(path, now, now); errors.Is(err, os.ErrNotExist) {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
	}
}
