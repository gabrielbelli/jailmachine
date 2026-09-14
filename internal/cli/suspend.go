package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sleeper"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// Suspend (ADR 0009). The machine's execution state is saved to its
// directory, every host process of the machine ends, and its memory goes
// back to the host; a wake restores it in seconds (resume.go).
const (
	// suspendSpareMiB is the free space a suspend needs on top of the
	// guest's memory, so a save never fills the volume.
	suspendSpareMiB = 2048
	// suspendGuestTimeout bounds each guest step of a suspend: the activity
	// probe and the quiesce script.
	suspendGuestTimeout = 30 * time.Second
	// rollbackTimeout bounds putting a machine back after a suspend that
	// did not go ahead; it runs even when the command was interrupted.
	rollbackTimeout = 2 * time.Minute
	// quiesceActive and quiesceBusy are quiesceScript's refusals: guest
	// activity, and a share that cannot be unmounted.
	quiesceActive = 3
	quiesceBusy   = 4
)

// freeBytes reports the space available to an unprivileged writer on the
// volume holding dir. A variable so tests can pretend.
var freeBytes = func(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func newSuspendCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "suspend [name]",
		Short: "Save a running machine to disk and give its memory back",
		Long: "Save the machine's complete running state to its directory and end its\n" +
			"processes, so its memory goes back to macOS. jpodman, jdocker, 'jm start' and\n" +
			"'jm ssh' wake it again in seconds, with every process in the guest as it was.\n" +
			"'jm stop' on a suspended machine restores it and shuts it down; 'jm stop\n" +
			"--force' and 'jm rm' discard the saved state.\n\n" +
			"A machine with jails or containers running, an engine client connected, a\n" +
			"command session open or /var/run/jm-nosleep present in the guest is not\n" +
			"suspended; --force suspends it anyway. A shared directory that is in use in\n" +
			"the guest always refuses, as does a volume without the guest's memory plus\n" +
			"2 GiB free. Published container ports do not wake a suspended machine.",
		Example: `  jm suspend
  jm suspend --force dev   # containers keep running after the wake`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			return runSuspend(cmd.Context(), m, force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "suspend with jails, containers, engine clients or sessions running (a share in use still refuses)")
	return cmd
}

// suspendOpts vary a suspend: who asked, and whether guest activity may be
// frozen.
type suspendOpts struct {
	// Reason is recorded in the journal ("jm suspend", "idle 30 min").
	Reason string
	// Manual is a user's "jm suspend": a busy lock is an error rather than
	// a skipped tick.
	Manual bool
	// Force ignores guest activity; a busy share still refuses.
	Force bool
}

// suspendRefusal is a suspend that did not go ahead, with the machine left as
// it was. It prints as its message and unwraps to the backend sentinel that
// classifies it (unavailable, blocked or aborted).
type suspendRefusal struct {
	kind error
	msg  string
}

func (e *suspendRefusal) Error() string { return e.msg }
func (e *suspendRefusal) Unwrap() error { return e.kind }

func refuse(kind error, format string, args ...any) error {
	return &suspendRefusal{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// detail strips a backend sentinel's own words from the front of err, for a
// message that already says what happened.
func detail(err error, sentinels ...error) string {
	msg := err.Error()
	for _, s := range sentinels {
		msg = strings.TrimPrefix(msg, s.Error()+": ")
	}
	return msg
}

// errSuspendSkipped is an automatic suspend that found the lock taken; the
// idle tracker keeps its time and tries again on the next tick.
var errSuspendSkipped = fmt.Errorf("suspend skipped: %w", machine.ErrLocked)

// runSuspend is "jm suspend". The suspend itself runs in the machine's
// sleeper, which is started if it is not alive: the sleeper must outlive the
// transition, because it holds the machine's endpoints while it is suspended.
func runSuspend(ctx context.Context, m *machine.Machine, force bool) error {
	b, p, err := components(m)
	if err != nil {
		return err
	}
	// The state is read without the lock for the messages; suspendMachine
	// reads it again under the lock before changing anything.
	st, err := stateOf(m, b, p)
	if err != nil {
		return err
	}
	switch st {
	case backend.Suspended:
		fmt.Fprintf(stdout, "%s is already suspended\n", m.Name)
		return nil
	case backend.Stopped:
		return withHint(fmt.Errorf("%s is not running", m.Name), "run 'jm start"+nameHint(m.Name)+"'")
	case backend.Broken:
		return withHint(fmt.Errorf("%s is broken", m.Name), "'jm stop"+nameHint(m.Name)+"' repairs it, 'jm start"+nameHint(m.Name)+"' repairs and starts it")
	}
	if err := suspendPreflight(m, b, p); err != nil {
		return err
	}
	logPath := sleeperProcess(m).LogPath()
	if err := startSleeper(ctx, m, b, p); err != nil {
		return withHint(fmt.Errorf("cannot suspend %s: %w", m.Name, err), "see "+logPath)
	}
	line := "suspend"
	if force {
		line += " force"
	}
	logf(stdout, "suspending %s; its sleeper holds the engine socket meanwhile (log: %s)", m.Name, logPath)
	began := time.Now()
	reply, err := sleeperRequest(ctx, m, line)
	if err != nil {
		return withHint(fmt.Errorf("cannot suspend %s: asking its sleeper: %w", m.Name, err), "see "+logPath)
	}
	word, rest := sleeper.SplitReply(reply)
	if word != "ok" {
		return suspendReplyError(m, reply)
	}
	image := ""
	if f := strings.Fields(rest); len(f) == 2 {
		if n, err := strconv.ParseInt(f[1], 10, 64); err == nil && n > 0 {
			image = "; image " + humanBytes(n) + " allocated"
		}
	}
	logf(stdout, "done: suspended %s in %.1f s%s", m.Name, time.Since(began).Seconds(), image)
	return nil
}

// suspendPreflight checks, without the lock and without changing anything,
// that m can be suspended at all: the backend and provider support it, the
// hypervisor was started in a way that can be saved, and the volume has
// room for the image.
func suspendPreflight(m *machine.Machine, b backend.Backend, p netprov.Provider) error {
	unavailable := func(format string, args ...any) error {
		return refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: %s", m.Name, fmt.Sprintf(format, args...))
	}
	s, ok := b.(backend.Suspender)
	if !ok || !b.Capabilities().Suspend {
		return unavailable("backend %q cannot save a running machine", b.Name())
	}
	if _, ok := p.(netprov.APIForwarder); !ok || !p.Capabilities().Supervised {
		return unavailable("network provider %q cannot be stopped and started around a suspend", p.Name())
	}
	if reason := s.Suspendable(m); reason != "" {
		return unavailable("%s", reason)
	}
	free, err := freeBytes(m.Dir)
	if err != nil {
		return unavailable("checking free space: %v", err)
	}
	need := uint64(m.MemoryMiB+suspendSpareMiB) << 20
	if free < need {
		return unavailable("need %s free on the volume holding %s, have %s", humanBytes(int64(need)), m.Dir, humanBytes(int64(free)))
	}
	return nil
}

// suspendMachine saves a running machine and ends its processes (ADR 0009,
// steps S1 to S16). The caller has run suspendPreflight. It never stops,
// repairs or discards a machine that was running: a suspend that does not go
// ahead puts the machine back as it was (the R-A and R-B rollbacks), and
// only a hypervisor that died during the save leaves it stopped (R-C).
//
// It runs inside the sleeper, whose stand-in sl takes the engine socket
// before the guest is quiesced (S6) and the SSH port once the provider is
// gone (S17). A client that arrives before the freeze cancels the suspend,
// and is relayed to the engine by the rollback.
func suspendMachine(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, sl *sleeper.Standin, opts suspendOpts) error {
	began := time.Now()
	s, ok := b.(backend.Suspender)
	if !ok {
		return refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: backend %q cannot save a running machine", m.Name, b.Name())
	}
	f, ok := p.(netprov.APIForwarder)
	if !ok {
		return refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: network provider %q cannot hand its endpoints over", m.Name, p.Name())
	}

	// From here until the freeze a suspend is cancellable: a wrapper that
	// finds the journal sends abort, and a client parked by the stand-in
	// counts as one. Armed before the journal exists, so no abort is lost.
	sl.Arm()
	defer sl.Disarm()

	// S1: lock, never waiting.
	unlock, err := store().Lock(m.Name)
	if errors.Is(err, machine.ErrLocked) {
		if !opts.Manual {
			return errSuspendSkipped
		}
		return fmt.Errorf("another jm command is operating on %q; try again shortly", m.Name)
	}
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := recoverInterruptedTransition(ctx, m, b, p); err != nil {
		return err
	}

	// S2: re-check under the lock.
	fresh, err := store().Load(m.Name)
	if err != nil {
		return err
	}
	m = fresh
	st, err := stateOf(m, b, p)
	if err != nil {
		return err
	}
	switch {
	case st == backend.Suspended:
		fmt.Fprintf(stdout, "%s is already suspended\n", m.Name)
		return nil
	case st != backend.Running:
		return withHint(fmt.Errorf("%s is %s", m.Name, st), "run 'jm start"+nameHint(m.Name)+"'")
	case suspendInProgress(m):
		return fmt.Errorf("a suspend or wake of %s is already in progress", m.Name)
	}
	ep, err := p.Endpoint(m)
	if err != nil {
		return err
	}
	dctx, dcancel := context.WithTimeout(ctx, suspendGuestTimeout)
	client, err := sshx.Dial(dctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	dcancel()
	if err != nil {
		return refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: the guest does not answer over ssh: %v", m.Name, err)
	}
	closeClient := func() {
		if client != nil {
			_ = client.Close()
			client = nil
		}
	}
	defer closeClient()
	act, perr := probeGuestActivity(ctx, client)
	if !opts.Force {
		if perr != nil {
			return refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: reading guest activity: %v", m.Name, perr)
		}
		baseline := 0
		if _, alive := forwarderProcess(m).Alive(); alive {
			baseline = 1 // the forwarder's own events stream
		}
		if blockers := act.blockers(baseline); len(blockers) > 0 {
			return withHint(refuse(backend.ErrSuspendBlocked, "%s is busy: %s", m.Name, strings.Join(blockers, ", ")),
				"'jm suspend --force"+nameHint(m.Name)+"' suspends it anyway")
		}
	}

	// S3 and S4: hypervisor preflight, then the journal.
	meta := map[string]string{
		"resolver_port": strconv.Itoa(resolverProcess(m).Port()),
		"arc_mib":       strconv.Itoa(m.ArcMiB),
		"mtu":           strconv.Itoa(m.MTU),
	}
	if perr == nil {
		meta["containers"] = strconv.Itoa(act.jails)
	}
	if sl.Aborted() {
		return refuse(backend.ErrSuspendAborted, "suspend of %s cancelled: a client arrived", m.Name)
	}
	logf(stdout, "suspending %s (%s)", m.Name, opts.Reason)
	if err := s.PrepareSuspend(ctx, m, backend.SuspendPlan{Reason: opts.Reason, Meta: meta}); err != nil {
		if errors.Is(err, backend.ErrSuspendUnavailable) {
			return refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: %s", m.Name, detail(err, backend.ErrSuspendUnavailable))
		}
		return fmt.Errorf("cannot suspend %s: %w", m.Name, err)
	}

	// From here a failure before the commit puts the machine back.
	rb := &suspendRollback{m: m, s: s, p: p, f: f, ep: ep, sl: sl, client: &client}

	// S5: the forwarder releases its mappings while the provider is up.
	stopForwarder(ctx, m, p)
	// S6: new engine clients reach the stand-in, which parks them unread;
	// each one cancels the suspend until the guest is frozen. The forward's
	// socket is kept aside until S8, so a rollback before then gives it back
	// with its live connections.
	if ep.APISocket != "" {
		if err := sl.TakeUnixKeepingPrevious(ep.APISocket); err != nil {
			return rb.guestRunning(ctx, fmt.Errorf("cannot suspend %s: holding the engine socket: %w", m.Name, err))
		}
	}
	// A wrapper's abort since the journal was written saves the quiesce.
	if sl.Aborted() {
		return rb.guestRunning(ctx, refuse(backend.ErrSuspendAborted, "suspend of %s cancelled: a client arrived", m.Name))
	}
	// S7: quiesce the guest.
	qctx, qcancel := context.WithTimeout(ctx, suspendGuestTimeout)
	out, code, err := runGuestScript(qctx, client, quiesceScript(opts.Force))
	qcancel()
	switch {
	case err != nil:
		return rb.guestRunning(ctx, fmt.Errorf("cannot suspend %s: quiescing the guest: %w", m.Name, err))
	case code == quiesceActive:
		return rb.guestRunning(ctx, withHint(refuse(backend.ErrSuspendBlocked, "%s is busy: %s", m.Name, quiesceReason(out, "active")),
			"'jm suspend --force"+nameHint(m.Name)+"' suspends it anyway"))
	case code == quiesceBusy:
		return rb.guestRunning(ctx, refuse(backend.ErrSuspendBlocked, "%s is busy: a shared directory is in use in the guest: %s", m.Name, quiesceReason(out, "busy")))
	case code != 0 || !strings.Contains(out, "ok"):
		return rb.guestRunning(ctx, fmt.Errorf("cannot suspend %s: quiescing the guest exited %d: %s", m.Name, code, lastLine(out)))
	}

	// S8: the engine socket forward goes. S7 found no engine client, and
	// the socket file is the stand-in's now, so it stays.
	if ep.APISocket != "" {
		if err := f.StopAPIForward(ctx, m); err != nil {
			return rb.guestRunning(ctx, fmt.Errorf("cannot suspend %s: stopping the engine socket forward: %w", m.Name, err))
		}
		rb.forwardStopped = true
		sl.DropPrevious()
	}
	// S9: a client that arrived since S6, or a wrapper's abort, cancels.
	if sl.Aborted() {
		return rb.guestRunning(ctx, refuse(backend.ErrSuspendAborted, "suspend of %s cancelled: a client arrived", m.Name))
	}

	// S10 to S14: the backend freezes, saves and commits. The ssh session
	// must not outlive the guest it talks to; the last abort check (S11)
	// runs immediately before the freeze.
	closeClient()
	logf(stdout, "saving %s's memory (%d MiB)", m.Name, m.MemoryMiB)
	if err := s.CommitSuspend(ctx, m, sl.CheckAndFreeze); err != nil {
		if cerr := rb.afterCommitError(ctx, b, err); cerr != nil {
			return cerr
		}
		fmt.Fprintf(stderr, "jm: warning: %s: %v; the state is saved\n", m.Name, err)
	}

	// S15 and S16: the provider and the resolver go; the resolver keeps its
	// port for the wake.
	if err := parkProvider(ctx, m, p); err != nil {
		fmt.Fprintf(stderr, "jm: warning: stopping %s networking: %v; continuing\n", m.Name, err)
	}
	stopResolver(ctx, m)
	// S17: the SSH port, once the provider has let go of it.
	addr := net.JoinHostPort(ep.SSHHost, strconv.Itoa(ep.SSHPort))
	if err := takeTCPWithin(ctx, sl, addr, takeTCPTimeout); err != nil {
		fmt.Fprintf(stderr, "jm: warning: ssh:// clients will not wake %s: %v\n", m.Name, err)
	}

	// S18.
	image := ""
	if status, err := s.SuspendStatus(m); err == nil && status.AllocatedBytes > 0 {
		image = "; image " + humanBytes(status.AllocatedBytes) + " allocated"
	}
	logf(stdout, "done: suspended %s in %.1f s%s (%s)", m.Name, time.Since(began).Seconds(), image, opts.Reason)
	return nil
}

// takeTCPWithin retries TakeTCP until the provider's exit has freed the port.
func takeTCPWithin(ctx context.Context, sl *sleeper.Standin, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := sl.TakeTCP(addr)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// suspendRollback puts a machine back when a suspend does not go ahead.
type suspendRollback struct {
	m      *machine.Machine
	s      backend.Suspender
	p      netprov.Provider
	f      netprov.APIForwarder
	ep     netprov.Endpoint
	sl     *sleeper.Standin
	client **sshx.Client
	// forwardStopped is set once S8 has stopped the engine socket forward.
	forwardStopped bool
}

// guestRunning is R-A: the guest was never frozen, or has been continued.
// Its shares are mounted again, the journal goes, the engine socket forward
// comes back and takes the socket from the stand-in, which relays the
// connections it parked, and the forwarder comes back. cause is returned.
func (r *suspendRollback) guestRunning(ctx context.Context, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	m := r.m
	if *r.client == nil {
		if c, err := sshx.Dial(ctx, r.ep.SSHHost, r.ep.SSHPort, m.SSHUser, sshKey(m)); err == nil {
			*r.client = c
		}
	}
	if c := *r.client; c != nil {
		remountSharesIfPending(ctx, m, c)
	} else {
		fmt.Fprintf(stderr, "jm: warning: %s: cannot reach the guest to mount its shares again; 'jm start%s' does it\n", m.Name, nameHint(m.Name))
	}
	if err := r.s.CancelSuspend(m); err != nil {
		return errors.Join(cause, fmt.Errorf("removing the suspend journal: %w", err))
	}
	if r.ep.APISocket != "" {
		// Before S8 the forward still serves the socket the stand-in took,
		// and clients may be streaming through it: put that socket back, so
		// the start below finds the forward serving and leaves it running.
		if !r.forwardStopped {
			r.sl.RestorePrevious()
		}
		if err := r.f.StartAPIForward(ctx, m); err != nil {
			fmt.Fprintf(stderr, "jm: warning: %s: the engine socket forward did not come back: %v; 'jm start%s' restores it\n", m.Name, err, nameHint(m.Name))
		}
	}
	r.sl.Disarm()
	if n := r.sl.HandOver(func() (net.Conn, error) {
		return net.DialTimeout("unix", r.ep.APISocket, handOverDialTimeout)
	}, func() (net.Conn, error) {
		return net.DialTimeout("tcp", net.JoinHostPort(r.ep.SSHHost, strconv.Itoa(r.ep.SSHPort)), handOverDialTimeout)
	}); n > 0 {
		logf(stdout, "handed %d waiting connection(s) to %s's engine", n, m.Name)
	}
	if err := startForwarder(m, r.p, r.ep); err != nil {
		fmt.Fprintf(stderr, "jm: warning: %v; 'jm start%s' restores it\n", err, nameHint(m.Name))
	}
	return cause
}

// afterCommitError sorts out a CommitSuspend that returned an error. It
// returns nil when the state was committed anyway (only ending the
// hypervisor went wrong), and otherwise the error to report after the
// rollback that applies: R-C when the hypervisor died, R-A when the guest
// runs, via recovery when it may still be frozen.
func (r *suspendRollback) afterCommitError(ctx context.Context, b backend.Backend, err error) error {
	m := r.m
	status, serr := r.s.SuspendStatus(m)
	if serr == nil && status.Phase == backend.SuspendSaved {
		return nil
	}
	bs, _ := b.State(m)
	if errors.Is(err, backend.ErrSuspendCrashed) || bs != backend.Running {
		return r.hypervisorDied(ctx, err)
	}
	if serr != nil {
		return withHint(fmt.Errorf("suspending %s: %w (the journal cannot be read: %v)", m.Name, err, serr),
			"'jm start"+nameHint(m.Name)+"' resolves the interrupted suspend")
	}
	switch {
	case errors.Is(err, backend.ErrSuspendBlocked):
		return r.guestRunning(ctx, refuse(backend.ErrSuspendBlocked, "cannot suspend %s: the hypervisor refuses to save it: %s", m.Name, detail(err, backend.ErrSuspendBlocked)))
	case errors.Is(err, backend.ErrSuspendAborted):
		return r.guestRunning(ctx, refuse(backend.ErrSuspendAborted, "suspend of %s cancelled: a client arrived", m.Name))
	case errors.Is(err, backend.ErrSuspendUnavailable):
		return r.guestRunning(ctx, refuse(backend.ErrSuspendUnavailable, "cannot suspend %s: %s", m.Name, detail(err, backend.ErrSuspendUnavailable)))
	}
	// The save itself failed (R-B). The backend continues the guest before
	// it returns; recovery makes sure of it, and never continues a
	// committed state, before the rest of R-A.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if _, rerr := r.s.Recover(rctx, m); rerr != nil {
		return withHint(fmt.Errorf("suspending %s failed (%w) and the guest could not be put back: %v", m.Name, err, rerr),
			"'jm start"+nameHint(m.Name)+"' resolves the interrupted suspend; "+consoleHint(m, b))
	}
	return r.guestRunning(ctx, fmt.Errorf("suspending %s failed; it is running again: %w", m.Name, err))
}

// hypervisorDied is R-C: the partial state goes, and so do the provider and
// the resolver. The disk is as the guest synced it before the freeze.
func (r *suspendRollback) hypervisorDied(ctx context.Context, cause error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	m := r.m
	if err := r.s.DiscardSuspend(m, "hypervisor exited while suspending"); err != nil {
		fmt.Fprintf(stderr, "jm: warning: %s: %v\n", m.Name, err)
	}
	// Nothing will wake a stopped machine for them.
	r.sl.Disarm()
	r.sl.CloseHeld()
	if err := parkProvider(ctx, m, r.p); err != nil {
		fmt.Fprintf(stderr, "jm: warning: %s: %v\n", m.Name, err)
	}
	stopResolver(ctx, m)
	return withHint(fmt.Errorf("hypervisor exited while suspending %s; the disk is consistent as of the pre-freeze sync: %w", m.Name, cause),
		"'jm start"+nameHint(m.Name)+"' boots it")
}

// runGuestScript runs script and returns its standard output and exit
// status; err is a transport failure only.
func runGuestScript(ctx context.Context, client *sshx.Client, script string) (string, int, error) {
	out, errOut, err := client.Run(ctx, script)
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return out + errOut, exit.ExitStatus(), nil
	}
	if err != nil {
		return out, -1, err
	}
	return out, 0, nil
}

// quiesceReason finds the "<word> ..." line a quiesce refusal printed.
func quiesceReason(out, word string) string {
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), word+" "); ok {
			return rest
		}
	}
	return lastLine(out)
}

// jmSessionsFn defines jm_sessions, which counts the command sessions in the
// guest: processes whose parent is an sshd session and which are not sshd
// themselves, less the shell running this very script. FreeBSD's ps takes
// everything after "=" as the header, commas included, so each keyword has
// its own -o.
const jmSessionsFn = `jm_sessions() {
  ps -ax -o pid= -o ppid= -o comm= | awk -v self=$$ '
    { pid[NR] = $1; ppid[NR] = $2; comm[$1] = $3; n = NR }
    END {
      s = 0
      for (i = 1; i <= n; i++)
        if (comm[ppid[i]] ~ /^sshd-sess/ && comm[pid[i]] !~ /^sshd/ && pid[i] != self) s++
      print s
    }'
}
`

// guestEngineClients counts sshd connections to the guest podman socket:
// every ssh -L channel, podman-remote over ssh://, the forwarder's events
// stream.
const guestEngineClients = `sockstat -u 2>/dev/null | awk '$2 ~ /^sshd/ && /-> \/var\/run\/podman\/po/' | wc -l | tr -d ' '`

// quiesceScript is S7: unless forced, refuse when the guest is in use
// (twice, a second apart); flush the file systems; save the list of 9p mounts
// and unmount them, children first and never forced. A share that will not
// unmount puts back what was unmounted and refuses. Exit 3 is activity,
// exit 4 a busy share; "ok" is success.
func quiesceScript(force bool) string {
	f := "0"
	if force {
		f = "1"
	}
	return "set -u; cd /\nJM_FORCE=" + f + "\n" + jmSessionsFn + jmRemountFn + `if [ "$JM_FORCE" != 1 ]; then
  [ ! -e ` + machine.GuestNoSleep + ` ] || { echo 'active inhibit'; exit 3; }
  for i in 1 2; do
    j=$(jls jid | wc -l | tr -d ' ')
    e=$(` + guestEngineClients + `)
    s=$(jm_sessions)
    [ "$j" = 0 ] && [ "$e" = 0 ] && [ "$s" = 0 ] || { echo "active jails=$j engine=$e sessions=$s"; exit 3; }
    [ $i = 1 ] && sleep 1
  done
fi
sync; zpool sync 2>/dev/null; sync
mount -p -t p9fs > ` + machine.GuestSuspendMounts + `
tail -r ` + machine.GuestSuspendMounts + ` | while read -r tag dir fs opts rest; do
  d=$(printf '%b' "$dir")
  umount "$d" || { echo "busy $d"; exit 4; }
done || { jm_remount; exit 4; }
echo ok
`
}

// activityProbe is the one-exec guest activity sample S2 takes.
const activityProbe = jmSessionsFn + `printf 'jails=%s\n' "$(jls jid | wc -l | tr -d ' ')"
printf 'engine=%s\n' "$(` + guestEngineClients + `)"
printf 'sessions=%s\n' "$(jm_sessions)"
printf 'inhibit=%s\n' "$([ -e ` + machine.GuestNoSleep + ` ] && echo 1 || echo 0)"
`

// guestActivity is one activityProbe sample.
type guestActivity struct {
	jails, engine, sessions int
	inhibit                 bool
}

// parseGuestActivity reads activityProbe's output; a missing or garbled key
// is an error, so a guest that cannot be read never looks idle.
func parseGuestActivity(out string) (guestActivity, error) {
	vals := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return guestActivity{}, fmt.Errorf("unexpected %q in the activity probe", l)
		}
		vals[k] = n
	}
	for _, k := range []string{"jails", "engine", "sessions", "inhibit"} {
		if _, ok := vals[k]; !ok {
			return guestActivity{}, fmt.Errorf("the activity probe did not report %s: %q", k, strings.TrimSpace(out))
		}
	}
	return guestActivity{jails: vals["jails"], engine: vals["engine"], sessions: vals["sessions"], inhibit: vals["inhibit"] != 0}, nil
}

// probeGuestActivity runs activityProbe.
func probeGuestActivity(ctx context.Context, client *sshx.Client) (guestActivity, error) {
	ctx, cancel := context.WithTimeout(ctx, suspendGuestTimeout)
	defer cancel()
	out, code, err := runGuestScript(ctx, client, activityProbe)
	if err != nil {
		return guestActivity{}, err
	}
	if code != 0 {
		return guestActivity{}, fmt.Errorf("the activity probe exited %d: %s", code, lastLine(out))
	}
	return parseGuestActivity(out)
}

// blockers lists what keeps the guest from being suspended, under the strict
// single-sample rules of a suspend that is about to happen. baseline is the
// number of engine clients jm itself holds.
func (a guestActivity) blockers(baseline int) []string {
	var out []string
	if a.jails > 0 {
		out = append(out, plural(a.jails, "1 jail or container running", strconv.Itoa(a.jails)+" jails or containers running"))
	}
	if n := a.engine - baseline; n > 0 {
		out = append(out, plural(n, "1 engine client connected", strconv.Itoa(n)+" engine clients connected"))
	}
	if a.sessions > 0 {
		out = append(out, plural(a.sessions, "1 command session open", strconv.Itoa(a.sessions)+" command sessions open"))
	}
	if a.inhibit {
		out = append(out, machine.GuestNoSleep+" exists")
	}
	return out
}
