package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/backend/qemu"
	"github.com/gabrielbelli/jailmachine/internal/idle"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/netprov/gvproxy"
	"github.com/gabrielbelli/jailmachine/internal/procx"
	"github.com/gabrielbelli/jailmachine/internal/sleeper"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// The sleeper (ADR 0009) is one detached helper per machine, alive whenever
// the machine is running or suspended and its backend and provider support
// suspend. It runs a suspend in its own process, because it must outlive the
// transition: while the machine is suspended it holds the engine socket and
// the SSH port. It never runs a wake itself; for a held connection it spawns
// "jm _wake", and every waker coordinates with it through the machine lock
// and the release-tcp request.
const (
	// sleeperHoldTick is how often the mode is re-evaluated while holding.
	// A monitoring sleeper re-evaluates on every idle probe tick instead.
	sleeperHoldTick = 250 * time.Millisecond
	// wakeSpawnBackoff is how long the sleeper waits before spawning
	// another "_wake" after one that left the machine suspended.
	wakeSpawnBackoff = 10 * time.Second
	// sleeperControlTimeout bounds a short control request (abort,
	// release-tcp, woke).
	sleeperControlTimeout = 2 * time.Second
	// releaseTCPTimeout bounds the wait for the SSH port after release-tcp.
	releaseTCPTimeout = 2 * time.Second
	releaseTCPPoll    = 20 * time.Millisecond
	// handOverDialTimeout bounds the dial of a real endpoint at hand-over.
	handOverDialTimeout = 5 * time.Second
)

// takeTCPTimeout bounds S17, taking the SSH port once the provider is gone.
// A variable so tests can shorten it.
var takeTCPTimeout = 5 * time.Second

// The idle monitor (ADR 0009). Variables so tests run it in milliseconds.
var (
	// idleProbeInterval is how often a monitoring sleeper samples.
	idleProbeInterval = idle.ProbeInterval
	// idleProbeTimeout bounds one guest probe, keepalive included.
	idleProbeTimeout = 10 * time.Second
	// idleMinute is the unit of idle_suspend_min.
	idleMinute = time.Minute
)

const (
	// idleProbeFailures is how many probes in a row may fail on the
	// persistent control connection before it is dialled again.
	idleProbeFailures = 3
	// idleSuspendFailures is how many automatic suspends in a row may fail
	// before automatic suspend is off until the sleeper restarts.
	idleSuspendFailures = 3
)

// hypervisorCPUTime is the cumulative CPU time of m's hypervisor process, from
// ps. A variable so tests can script it.
var hypervisorCPUTime = func(m *machine.Machine, b backend.Backend) (time.Duration, error) {
	if b.Name() != qemu.Name || m.Dir == "" {
		return 0, fmt.Errorf("no hypervisor CPU time for backend %q", b.Name())
	}
	data, err := os.ReadFile(filepath.Join(m.Dir, qemu.PIDFile))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("bad pid file %s", qemu.PIDFile)
	}
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	return idle.ParseCPUTime(string(out))
}

// sleeperProcess locates m's sleeper. The sleeper and its wakers keep the
// variable that locates the network provider's program: a wake starts it as
// a start does.
func sleeperProcess(m *machine.Machine) sleeper.Process {
	return sleeper.Process{Dir: m.Dir, Name: m.Name, Root: StateRoot(), KeepEnv: []string{gvproxy.BinaryEnv}}
}

// sleeperAlive reports whether m's sleeper runs; a variable so tests can
// pretend.
var sleeperAlive = func(m *machine.Machine) bool {
	_, ok := sleeperProcess(m).Alive()
	return ok
}

// sleeperSupported reports whether m's components can suspend at all; a
// machine whose backend or provider cannot has no sleeper.
func sleeperSupported(b backend.Backend, p netprov.Provider) bool {
	if _, ok := b.(backend.Suspender); !ok || !b.Capabilities().Suspend {
		return false
	}
	if _, ok := p.(netprov.APIForwarder); !ok || !p.Capabilities().Supervised {
		return false
	}
	return true
}

// launchSleeper starts m's sleeper and waits for its control socket. A
// variable so tests never launch a process.
var launchSleeper = func(ctx context.Context, m *machine.Machine) error {
	exe, err := jmBinary()
	if err != nil {
		return fmt.Errorf("locating the jm binary: %w", err)
	}
	return sleeperProcess(m).Start(ctx, exe)
}

// stopSleeperProcess stops m's sleeper; a variable so tests can see it.
var stopSleeperProcess = func(ctx context.Context, m *machine.Machine) error {
	return sleeperProcess(m).Stop(ctx)
}

// sleeperRequest sends one control line to m's sleeper. A variable so tests
// can answer with an in-process sleeper.
var sleeperRequest = func(ctx context.Context, m *machine.Machine, line string) (string, error) {
	return sleeper.Request(ctx, sleeperProcess(m).ControlPath(), line)
}

// tellSleeper sends a short request and reports the reply; ok is false when
// no sleeper answered.
func tellSleeper(ctx context.Context, m *machine.Machine, line string) (reply string, ok bool) {
	if m.Dir == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, sleeperControlTimeout)
	defer cancel()
	reply, err := sleeperRequest(ctx, m, line)
	return reply, err == nil
}

// startSleeper is the sleeper stage of "jm start": launch the helper unless
// it is alive. It takes no lock, so a sleeper started under a command's lock
// never waits for it.
func startSleeper(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider) error {
	if m.Dir == "" || !sleeperSupported(b, p) {
		return nil
	}
	pr := sleeperProcess(m)
	if _, ok := pr.Alive(); ok {
		return nil
	}
	logf(stdout, "%s: starting the sleeper, which holds the machine's endpoints while it is suspended (log: %s)", machine.StageSleeper, pr.LogPath())
	if err := launchSleeper(ctx, m); err != nil {
		return machine.NewStageError(machine.StageSleeper, "see "+pr.LogPath(), err)
	}
	return nil
}

// stopSleeper terminates the sleeper; a stopped or absent one is tidied
// away. It runs first in "jm stop" and "jm rm": a sleeper holding a suspended
// machine's endpoints gives them up before the machine is restored or
// discarded.
func stopSleeper(ctx context.Context, m *machine.Machine) {
	if m.Dir == "" {
		return
	}
	if err := stopSleeperProcess(ctx, m); err != nil {
		fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
	}
}

// releaseSleeperTCP is W1: a sleeper holding the SSH port lets go of it for
// this process, which is about to start the provider on it. No sleeper means
// nothing of jm's holds the port.
func releaseSleeperTCP(ctx context.Context, m *machine.Machine, ep netprov.Endpoint) error {
	reply, ok := tellSleeper(ctx, m, "release-tcp "+strconv.Itoa(os.Getpid()))
	if !ok {
		return nil
	}
	if word, rest := sleeper.SplitReply(reply); word != "ok" {
		return fmt.Errorf("the sleeper did not release port %d: %s", ep.SSHPort, rest)
	}
	addr := net.JoinHostPort(ep.SSHHost, strconv.Itoa(ep.SSHPort))
	deadline := time.Now().Add(releaseTCPTimeout)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d is in use", ep.SSHPort)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(releaseTCPPoll):
		}
	}
}

// abortSuspendInFlight asks the sleeper to cancel a suspend that has not
// frozen the guest yet, for a client that wants the engine now. It does
// nothing unless the journal says the suspend is still saving.
func abortSuspendInFlight(ctx context.Context, m *machine.Machine) {
	b, err := backendFor(m)
	if err != nil {
		return
	}
	s, ok := b.(backend.Suspender)
	if !ok {
		return
	}
	if st, err := s.SuspendStatus(m); err != nil || st.Phase != backend.SuspendSaving {
		return
	}
	tellSleeper(ctx, m, "abort")
}

func newSleeperCmd() *cobra.Command {
	return &cobra.Command{
		Use:    sleeper.Command + " [name]",
		Short:  "Hold a machine's endpoints while it is suspended (internal)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			return runSleeper(cmd.Context(), m)
		},
	}
}

// runSleeper is the sleeper's main: one instance per machine (the pid file
// lock), a control socket, and the mode loop until the machine is stopped or
// the sleeper is told to end.
func runSleeper(ctx context.Context, m *machine.Machine) error {
	b, p, err := components(m)
	if err != nil {
		return err
	}
	if !sleeperSupported(b, p) {
		return fmt.Errorf("%s: backend %q or network %q cannot suspend; no sleeper needed", m.Name, b.Name(), p.Name())
	}
	logger := log.New(stdout, "sleeper: ", log.LstdFlags)
	pr := sleeperProcess(m)
	release, err := pr.LockPID()
	if errors.Is(err, sleeper.ErrAlreadyRunning) {
		logger.Printf("another sleeper holds %s; exiting", sleeper.PIDFile)
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	ln, err := sleeper.Listen(pr.ControlPath())
	if errors.Is(err, sleeper.ErrAlreadyRunning) {
		logger.Printf("another sleeper answers on %s; exiting", pr.ControlPath())
		return nil
	}
	if err != nil {
		return err
	}
	defer os.Remove(pr.ControlPath())

	exe, err := jmBinary()
	if err != nil {
		return fmt.Errorf("locating the jm binary: %w", err)
	}
	d := newSleeperDaemon(m, b, p, logger)
	d.waker.Spawn = func() (int, error) {
		return procx.StartDetached(exe, pr.WakeArgs(), sleeper.ChildEnv(os.Environ(), pr.KeepEnv...), pr.WakeLogPath(), false)
	}
	serveCtx, stopServing := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = sleeper.Serve(serveCtx, ln, d.handle)
	}()
	logger.Printf("started for %s (pid %d, control %s)", m.Name, os.Getpid(), pr.ControlPath())
	d.loop(ctx)
	// No suspend runs any more (loop waited for it and refuses new ones);
	// the requests being answered finish before the stand-in closes.
	stopServing()
	<-served
	d.shutdown()
	logger.Printf("stopped")
	return nil
}

// sleeperDaemon is the state of one running sleeper.
type sleeperDaemon struct {
	m     *machine.Machine
	b     backend.Backend
	p     netprov.Provider
	pr    sleeper.Process
	sl    *sleeper.Standin
	waker *sleeper.Waker
	log   *log.Logger
	nudge chan struct{}

	// suspending is set while a suspend runs; the mode loop leaves the
	// endpoints to it. suspendWG counts it, and closing (under mu, like
	// the Add) refuses new ones once the loop waits for it.
	suspending atomic.Bool
	suspendWG  sync.WaitGroup

	mu            sync.Mutex
	closing       bool
	status        sleeper.Status
	trigger       sleeper.Kind // the endpoint the first held connection used
	stoppedBefore bool
	takeWarned    map[string]bool
	// triggerAt is when the first connection arrived for a wake, and wokeAt
	// when a waker's "woke" says its wake began; the flap guard reads the
	// earlier. Both are cleared when a suspend starts.
	triggerAt, wokeAt time.Time

	// mon is the idle monitor; only the mode loop's goroutine uses it.
	mon idleMonitor
	// probeMu guards client, the persistent control connection the idle
	// probe runs on. A suspend closes it before its own probe, so the two
	// never count each other as a session.
	probeMu        sync.Mutex
	client         *sshx.Client
	clientFailures int
}

// idleMonitor is the state of the idle monitor between ticks.
type idleMonitor struct {
	// now is the clock, a variable for tests; its readings carry the
	// monotonic clock, so a sample's elapsed time never follows the wall
	// clock.
	now func() time.Time
	// active is whether the sleeper is monitoring; entering monitor mode
	// restarts the tracker.
	active  bool
	tracker idle.Tracker
	// lastProbe is when the last probe ran, lastSample when the last sample
	// (or monitoring) began: a sample's elapsed time is measured from it.
	lastProbe, lastSample time.Time
	// activity is the activity file's mtime at the last sample, and
	// activityKnown whether there was a last sample to compare with.
	activity      time.Time
	activityKnown bool
	// cpu is the hypervisor's CPU time at cpuAt, when cpuKnown.
	cpu      time.Duration
	cpuAt    time.Time
	cpuKnown bool
	// failures counts automatic suspends that failed in a row; disabled
	// is set once there were too many, and holds until the sleeper exits.
	failures int
	disabled string
	// refusal explains why the last automatic suspend was refused when no
	// probe can see it (a share the guest would not unmount, the
	// hypervisor's own reasons). It is shown with the blockers until the
	// next attempt, a probe that finds blockers of its own, or a restart.
	refusal string
	// suspendedAt is when this sleeper's last automatic suspend committed;
	// zero once the flap guard has seen its wake.
	suspendedAt time.Time
}

func newSleeperDaemon(m *machine.Machine, b backend.Backend, p netprov.Provider, logger *log.Logger) *sleeperDaemon {
	d := &sleeperDaemon{
		m: m, b: b, p: p, pr: sleeperProcess(m), log: logger,
		sl:         &sleeper.Standin{},
		nudge:      make(chan struct{}, 1),
		takeWarned: map[string]bool{},
	}
	d.status = sleeper.Status{Mode: sleeper.ModeMonitor, SSHWake: true}
	d.mon.now = time.Now
	d.sl.OnAccept = d.accepted
	d.waker = &sleeper.Waker{Alive: procx.Alive, Backoff: wakeSpawnBackoff}
	return d
}

// wake re-evaluates the mode now.
func (d *sleeperDaemon) wake() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// accepted is the stand-in's accept callback: remember what the first held
// connection came in on, and re-evaluate at once. The stand-in itself sets
// the abort flag of a suspend before its freeze.
func (d *sleeperDaemon) accepted(k sleeper.Kind) {
	d.mu.Lock()
	if d.trigger == "" {
		d.trigger = k
	}
	if d.triggerAt.IsZero() {
		d.triggerAt = time.Now()
	}
	d.mu.Unlock()
	d.wake()
}

// update changes the status and writes sleeper.json.
func (d *sleeperDaemon) update(f func(*sleeper.Status)) {
	d.mu.Lock()
	if f != nil {
		f(&d.status)
	}
	d.status.PID = os.Getpid()
	d.status.UpdatedAt = time.Now()
	d.status.Held = d.sl.Held()
	st := d.status
	d.mu.Unlock()
	if d.m.Dir == "" {
		return
	}
	if err := sleeper.WriteStatus(d.pr.StatusPath(), st); err != nil {
		d.log.Printf("writing %s: %v", d.pr.StatusPath(), err)
	}
}

// loop evaluates the mode table until the machine is stopped or ctx ends. A
// suspend in flight finishes first either way: it holds the lock and the
// machine is between states until it returns.
func (d *sleeperDaemon) loop(ctx context.Context) {
	defer d.suspendWG.Wait()
	defer d.closeSuspends()
	for {
		mode := d.evaluate()
		if mode == sleeper.ModeExit {
			d.log.Printf("%s is stopped; exiting", d.m.Name)
			return
		}
		tick := sleeperHoldTick
		if mode == sleeper.ModeMonitor {
			tick = d.monitor(ctx)
		} else {
			d.leaveMonitor()
		}
		select {
		case <-ctx.Done():
			return
		case <-d.nudge:
		case <-time.After(tick):
		}
	}
}

// closeSuspends refuses every suspend from now on.
func (d *sleeperDaemon) closeSuspends() {
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
}

// tryLock takes the machine lock without waiting. busy is true when another
// command holds it; otherwise unlock releases what was taken. A machine
// whose directory is gone is never locked, so its directory is not made
// again.
func (d *sleeperDaemon) tryLock() (unlock func(), busy bool) {
	if _, err := os.Stat(d.m.Dir); err != nil {
		return func() {}, false
	}
	unlock, err := store().Lock(d.m.Name)
	if errors.Is(err, machine.ErrLocked) {
		return nil, true
	}
	if err != nil {
		return func() {}, false
	}
	return unlock, false
}

// wakerAlive reports whether a wake this sleeper knows of runs: its own
// "_wake" child, or the process the SSH port was released to.
func (d *sleeperDaemon) wakerAlive() bool {
	if d.waker.Running() != 0 {
		return true
	}
	pid := d.sl.ReleasedTo()
	return pid != 0 && procx.Alive(pid)
}

// transitionInFlight reports whether a command may be between states now: a
// waker lives or the machine lock is taken.
func (d *sleeperDaemon) transitionInFlight() bool {
	if d.wakerAlive() {
		return true
	}
	unlock, busy := d.tryLock()
	if !busy {
		unlock()
	}
	return busy
}

// giveUpUnservedSocket stops holding the engine socket of a machine that
// runs, with no journal, while the socket is still the stand-in's: a wake
// that continued the guest and then failed before the engine socket forward
// took the path back (W6 to W8). Held on, every client would wait for a
// hand-over that never comes, and a wrapper would take the answering socket
// for a ready engine. Unlinked, clients are refused and a wrapper or "jm
// start" restores the forward. A wake still in flight is left alone; the
// check runs under the machine lock, so none can start meanwhile.
func (d *sleeperDaemon) giveUpUnservedSocket() {
	if d.wakerAlive() {
		return
	}
	unlock, busy := d.tryLock()
	if busy {
		return
	}
	defer unlock()
	if suspendInProgress(d.m) || !d.sl.UnixIsOurs() {
		return
	}
	if st, err := stateOf(d.m, d.b, d.p); err != nil || st != backend.Running {
		return
	}
	path := d.sl.UnixPath()
	if err := d.sl.CloseUnix(true); err != nil {
		d.log.Printf("removing %s: %v", path, err)
	}
	d.log.Printf("%s runs, but no wake took its engine socket back; stopped holding %s ('jm start%s' restores the forward)", d.m.Name, path, nameHint(d.m.Name))
}

// evaluate is one pass of the mode table (ADR 0009).
func (d *sleeperDaemon) evaluate() sleeper.Mode {
	if d.suspending.Load() {
		return sleeper.ModeHold
	}
	m := d.m
	st, err := stateOf(m, d.b, d.p)
	if err != nil {
		st = backend.Broken
	}
	journal := suspendInProgress(m)

	if d.waker.Reap() && st == backend.Suspended {
		n := d.sl.CloseHeld()
		d.log.Printf("the wake left %s suspended; closed %d held connection(s); next attempt in %s (see %s)", m.Name, n, wakeSpawnBackoff, d.pr.WakeLogPath())
		d.mu.Lock()
		d.trigger = ""
		d.mu.Unlock()
		d.waker.StartBackoff()
	}
	if st == backend.Suspended {
		if ok, err := d.sl.RelistenIfOwnerGone(procx.Alive); ok {
			d.log.Printf("the process the SSH port was released to is gone; holding %s again", d.sl.TCPAddr())
		} else if err != nil {
			d.log.Printf("taking the SSH port back: %v", err)
		}
	}

	if st == backend.Running && !journal && d.sl.UnixIsOurs() {
		d.giveUpUnservedSocket()
	}

	obs := sleeper.Observation{State: st, Journal: journal, Holding: d.sl.Holding(), StoppedBefore: d.stoppedBefore}
	if obs.Stopped() && obs.StoppedBefore {
		// A wake that falls back to a cold boot reads stopped for a moment.
		obs.Busy = d.transitionInFlight()
	}
	if obs.Holding && !journal && st == backend.Running {
		obs.EndpointsBack = d.sl.EndpointsBack()
	}
	mode := sleeper.Decide(obs)
	d.stoppedBefore = obs.Stopped()
	switch mode {
	case sleeper.ModeHold:
		if st == backend.Suspended {
			d.takeEndpoints()
			d.wakeForHeld()
		}
	case sleeper.ModeHandOver:
		d.handOver()
		mode = sleeper.ModeMonitor
	}
	d.mu.Lock()
	changed := d.status.Mode != mode || d.status.Held != d.sl.Held()
	d.mu.Unlock()
	if changed {
		d.update(func(s *sleeper.Status) { s.Mode = mode })
	}
	return mode
}

// takeEndpoints holds a suspended machine's endpoints that nobody serves.
// While the provider runs a wake is past W2 and owns them, so nothing is
// taken then.
func (d *sleeperDaemon) takeEndpoints() {
	m := d.m
	if ps, err := d.p.State(m); err != nil || ps != backend.Stopped {
		return
	}
	ep, err := d.p.Endpoint(m)
	if err != nil {
		return
	}
	if ep.APISocket != "" && !d.sl.UnixIsOurs() {
		if err := d.sl.TakeUnix(ep.APISocket); err != nil {
			d.warnOnce("unix", "cannot hold %s: %v; engine socket clients will not wake %s", ep.APISocket, err, m.Name)
		} else {
			d.takeWarned["unix"] = false
			d.log.Printf("holding %s", ep.APISocket)
		}
	}
	if d.sl.TCPListening() {
		return
	}
	if pid := d.sl.ReleasedTo(); pid != 0 && procx.Alive(pid) {
		return
	}
	addr := net.JoinHostPort(ep.SSHHost, strconv.Itoa(ep.SSHPort))
	if err := d.sl.TakeTCP(addr); err != nil {
		if d.warnOnce("tcp", "cannot hold %s: %v; ssh:// clients will not wake %s", addr, err, m.Name) {
			d.update(func(s *sleeper.Status) { s.SSHWake = false })
		}
		return
	}
	d.takeWarned["tcp"] = false
	d.log.Printf("holding %s", addr)
	d.update(func(s *sleeper.Status) { s.SSHWake = true })
}

// warnOnce logs a failure the first time it happens in a row, and reports
// whether it did.
func (d *sleeperDaemon) warnOnce(key, format string, args ...any) bool {
	if d.takeWarned[key] {
		return false
	}
	d.takeWarned[key] = true
	d.log.Printf(format, args...)
	return true
}

// wakeForHeld spawns a "_wake" for held connections, one at a time.
func (d *sleeperDaemon) wakeForHeld() {
	if d.sl.Held() == 0 {
		return
	}
	spawned, err := d.waker.Ensure()
	switch {
	case err != nil:
		d.log.Printf("spawning %s for %s: %v; next attempt in %s", sleeper.WakeCommand, d.m.Name, err, wakeSpawnBackoff)
	case spawned:
		d.log.Printf("a connection arrived; waking %s (%s pid %d, log %s)", d.m.Name, sleeper.WakeCommand, d.waker.Running(), d.pr.WakeLogPath())
	}
}

// handOver relays every held connection to the real endpoints and steps out.
func (d *sleeperDaemon) handOver() {
	ep, err := d.p.Endpoint(d.m)
	if err != nil {
		d.log.Printf("reading the endpoints for the hand-over: %v", err)
		return
	}
	addr := net.JoinHostPort(ep.SSHHost, strconv.Itoa(ep.SSHPort))
	n := d.sl.HandOver(func() (net.Conn, error) {
		return net.DialTimeout("unix", ep.APISocket, handOverDialTimeout)
	}, func() (net.Conn, error) {
		return net.DialTimeout("tcp", addr, handOverDialTimeout)
	})
	d.mu.Lock()
	trigger := d.trigger
	d.trigger = ""
	d.mu.Unlock()
	d.log.Printf("%s is awake; handed %d held connection(s) over", d.m.Name, n)
	d.update(func(s *sleeper.Status) {
		s.Mode, s.State, s.SuspendedAt = sleeper.ModeMonitor, "", nil
		if s.LastWakeBy == "" && trigger != "" {
			s.LastWakeBy = wakeByKind(trigger)
		}
	})
}

// wakeByKind names a held connection's endpoint for last_wake_by.
func wakeByKind(k sleeper.Kind) string {
	if k == sleeper.TCP {
		return "ssh-port"
	}
	return "socket"
}

// shutdown lets go of everything: held connections close, and the engine
// socket is unlinked only while it is still the stand-in's own.
func (d *sleeperDaemon) shutdown() {
	if err := d.sl.Close(); err != nil {
		d.log.Printf("closing the stand-in: %v", err)
	}
}

// handle answers one control request.
func (d *sleeperDaemon) handle(ctx context.Context, line string) string {
	f := sleeper.Fields(line)
	switch f[0] {
	case "abort":
		d.sl.Abort()
		return "ok"
	case "release-tcp":
		if len(f) != 2 {
			return "err release-tcp needs a pid"
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil || pid <= 0 {
			return "err bad pid " + strconv.Quote(f[1])
		}
		if err := d.sl.ReleaseTCP(pid); err != nil {
			return "err " + err.Error()
		}
		if d.sl.TCPAddr() != "" {
			d.log.Printf("released %s to pid %d", d.sl.TCPAddr(), pid)
		}
		return "ok"
	case "woke":
		by, ms := "", int64(0)
		if len(f) > 1 && f[1] != "-" {
			by = f[1]
		}
		if len(f) > 2 {
			ms, _ = strconv.ParseInt(f[2], 10, 64)
		}
		d.mu.Lock()
		if by == "" && d.trigger != "" {
			by = wakeByKind(d.trigger)
		}
		if d.wokeAt.IsZero() {
			d.wokeAt = time.Now().Add(-time.Duration(ms) * time.Millisecond)
		}
		d.mu.Unlock()
		d.update(func(s *sleeper.Status) {
			s.LastWakeBy = by
			if ms > 0 {
				s.LastResumeMS = ms
			}
		})
		d.wake()
		return "ok"
	case "suspend":
		force := len(f) > 1 && f[1] == "force"
		return d.suspend(ctx, force)
	case "status":
		d.mu.Lock()
		data, err := json.Marshal(d.status)
		d.mu.Unlock()
		if err != nil {
			return "err " + err.Error()
		}
		return string(data)
	}
	return "err unknown request"
}

// Why a suspend did not start in this sleeper at all.
var (
	errSleeperStopping = errors.New("the sleeper is stopping")
	errSuspendRunning  = errors.New("a suspend is already running")
)

// suspend runs "jm suspend" for the command that asked, and replies with the
// outcome. It is never cancelled by the requester going away: a suspend that
// has started runs to its commit or its rollback.
func (d *sleeperDaemon) suspend(ctx context.Context, force bool) string {
	ms, alloc, err := d.runSuspend(ctx, suspendOpts{Reason: "jm suspend", Manual: true, Force: force})
	switch {
	case errors.Is(err, errSleeperStopping):
		return "err the sleeper of " + d.m.Name + " is stopping"
	case errors.Is(err, errSuspendRunning):
		return "busy a suspend of " + d.m.Name + " is already running"
	case err != nil:
		return suspendReply(err)
	}
	return fmt.Sprintf("ok %d %d", ms, alloc)
}

// runSuspend runs one suspend in this sleeper, for "jm suspend" or the idle
// monitor: only one at a time, and none once the sleeper is stopping. It
// returns how long it took and the image's allocated size.
func (d *sleeperDaemon) runSuspend(ctx context.Context, opts suspendOpts) (ms, alloc int64, err error) {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return 0, 0, errSleeperStopping
	}
	if !d.suspending.CompareAndSwap(false, true) {
		d.mu.Unlock()
		return 0, 0, errSuspendRunning
	}
	d.suspendWG.Add(1)
	// A wake is measured from triggers that arrive from here on.
	d.triggerAt, d.wokeAt = time.Time{}, time.Time{}
	d.mu.Unlock()
	// A suspend starts from a running machine, so any waker on record did its
	// job; forget it before the machine reads suspended again. A suspend that
	// follows a wake within one idle tick would otherwise find that exited
	// waker, count it as a failed wake and hold new clients for the backoff.
	d.waker.Forget()
	defer func() {
		d.suspendWG.Done()
		d.suspending.Store(false)
		d.wake()
	}()
	// The idle probe's session would count as a command session in the
	// suspend's own probe: it waits for one in flight and closes the
	// connection, which is never dialled again until the sleeper monitors.
	d.closeProbeClient()
	ctx = context.WithoutCancel(ctx)
	m, err := store().Load(d.m.Name)
	if err != nil {
		return 0, 0, err
	}
	began := time.Now()
	if err := suspendPreflight(m, d.b, d.p); err != nil {
		return 0, 0, err
	}
	if err := suspendMachine(ctx, m, d.b, d.p, d.sl, opts); err != nil {
		if !errors.Is(err, machine.ErrLocked) {
			d.update(func(s *sleeper.Status) { s.LastSuspendError = err.Error() })
		}
		return 0, 0, err
	}
	ms = time.Since(began).Milliseconds()
	var savedAt time.Time
	if s, ok := d.b.(backend.Suspender); ok {
		if status, err := s.SuspendStatus(m); err == nil {
			alloc, savedAt = status.AllocatedBytes, status.SavedAt
		}
	}
	if savedAt.IsZero() {
		savedAt = time.Now()
	}
	d.update(func(s *sleeper.Status) {
		s.State, s.SuspendedAt = string(backend.Suspended), &savedAt
		s.LastSuspendMS, s.LastSuspendError, s.LastWakeBy = ms, "", ""
		// S17 may have failed to take the SSH port: ssh:// clients then
		// get connection refused rather than a wake.
		s.SSHWake = d.sl.TCPListening()
	})
	return ms, alloc, nil
}

// monitor is one pass of monitor mode: an idle sample when one is due, and
// an automatic suspend when the machine has been idle long enough. It
// returns how long to wait for the next pass.
func (d *sleeperDaemon) monitor(ctx context.Context) time.Duration {
	mon := &d.mon
	now := mon.now()
	if !mon.active {
		d.enterMonitor(now)
	}
	// A control request re-evaluates the mode early; the probe keeps its
	// own interval, so consecutive samples stay about an interval apart.
	if !mon.lastProbe.IsZero() {
		if wait := idleProbeInterval - now.Sub(mon.lastProbe); wait > idleProbeInterval/10 {
			return wait
		}
	}
	mon.lastProbe = now
	d.idleTick(ctx, now)
	return idleProbeInterval
}

// enterMonitor starts monitoring: the idle time starts from zero (after a
// boot, a wake or a rollback), and a wake that follows this sleeper's
// automatic suspend is shown to the flap guard.
func (d *sleeperDaemon) enterMonitor(now time.Time) {
	mon := &d.mon
	mon.active = true
	mon.tracker.Interval = idleProbeInterval
	mon.tracker.Restart()
	mon.lastProbe, mon.lastSample = time.Time{}, now
	mon.activityKnown, mon.cpuKnown = false, false
	mon.refusal = ""
	if mon.suspendedAt.IsZero() {
		return
	}
	d.mu.Lock()
	trigger := d.triggerAt
	if trigger.IsZero() || (!d.wokeAt.IsZero() && d.wokeAt.Before(trigger)) {
		trigger = d.wokeAt
	}
	d.triggerAt, d.wokeAt = time.Time{}, time.Time{}
	d.mu.Unlock()
	if trigger.IsZero() {
		trigger = time.Now()
	}
	// Wall clock: a Mac asleep with the machine suspended counts as asleep.
	asleep := trigger.Round(0).Sub(mon.suspendedAt.Round(0))
	before := mon.tracker.Penalty()
	mon.tracker.Woke(asleep)
	mon.suspendedAt = time.Time{}
	if after := mon.tracker.Penalty(); after != before {
		d.log.Printf("%s woke %s after its idle suspend; idle period is now %d× the setting", d.m.Name, asleep.Round(time.Second), after)
	}
}

// leaveMonitor stops monitoring: the control connection closes (it is never
// dialled while the sleeper holds endpoints or the machine is suspended), and
// the idle fields leave sleeper.json.
func (d *sleeperDaemon) leaveMonitor() {
	if !d.mon.active {
		return
	}
	d.mon.active = false
	d.closeProbeClient()
	d.update(func(s *sleeper.Status) {
		s.IdleSeconds, s.IdleSuspendAfterSeconds, s.Blockers = 0, 0, nil
		s.IdleUnavailable, s.CPUPercent = "", 0
	})
}

// idleTick takes one sample, records it in sleeper.json and suspends the
// machine when it is due. The record is read on every tick, so "jm set
// --idle-suspend" applies at once.
func (d *sleeperDaemon) idleTick(ctx context.Context, now time.Time) {
	mon := &d.mon
	m, err := store().Load(d.m.Name)
	if err != nil {
		return
	}
	sample, perr := d.probeGuest(ctx, m)
	sample.EngineBaseline = engineBaseline(m)
	d.hostSignals(m, &sample)
	elapsed := now.Sub(mon.lastSample)
	mon.lastSample = now
	before := mon.tracker.Blockers()
	mon.tracker.Observe(sample, perr, elapsed)
	if perr == nil && len(mon.tracker.Blockers()) > 0 {
		mon.refusal = ""
	}
	if after := mon.tracker.Blockers(); !slices.Equal(before, after) {
		if len(after) == 0 {
			d.log.Printf("%s is idle", m.Name)
		} else {
			d.log.Printf("%s is held awake by: %s", m.Name, strings.Join(after, ", "))
		}
	}

	period := time.Duration(m.IdleSuspendMin) * idleMinute
	if period <= 0 {
		mon.refusal = ""
	}
	unavailable := ""
	if err := suspendPreflight(m, d.b, d.p); err != nil {
		unavailable = strings.TrimPrefix(err.Error(), "cannot suspend "+m.Name+": ")
	}
	due := mon.tracker.Due(period) && unavailable == "" && mon.disabled == ""
	d.writeIdleStatus(m, period, unavailable, sample)
	if due {
		d.autoSuspend(ctx, m)
		d.writeIdleStatus(m, period, unavailable, sample)
	}
}

// writeIdleStatus puts the idle monitor's view into sleeper.json.
func (d *sleeperDaemon) writeIdleStatus(m *machine.Machine, period time.Duration, unavailable string, sample idle.Sample) {
	mon := &d.mon
	d.update(func(s *sleeper.Status) {
		s.IdleSeconds = int64(mon.tracker.Idle() / time.Second)
		s.IdleSuspendAfterSeconds = int64(mon.tracker.Threshold(period) / time.Second)
		s.Blockers = mon.tracker.Blockers()
		if mon.refusal != "" {
			s.Blockers = append(s.Blockers, mon.refusal)
		}
		s.LastActivity = nil
		if !mon.activity.IsZero() {
			at := mon.activity
			s.LastActivity = &at
		}
		s.IdleUnavailable, s.DisabledReason = unavailable, mon.disabled
		s.Penalty = mon.tracker.Penalty()
		s.CPUPercent = 0
		if sample.CPUKnown {
			s.CPUPercent = float64(int(sample.CPUPercent*10+0.5)) / 10
		}
	})
}

// autoSuspend is the idle timer firing. A lock held by another command skips
// this tick with the idle time kept; guest activity or a client found on the
// way resets it; any other failure backs off a full idle period, and
// idleSuspendFailures of them in a row turn automatic suspend off until the
// sleeper restarts.
func (d *sleeperDaemon) autoSuspend(ctx context.Context, m *machine.Machine) {
	mon := &d.mon
	idleFor := mon.tracker.Idle().Round(time.Second)
	reason := "idle " + idleSuspendWord(m.IdleSuspendMin)
	d.log.Printf("%s has been idle for %s; suspending it", m.Name, idleFor)
	mon.refusal = ""
	_, _, err := d.runSuspend(ctx, suspendOpts{Reason: reason})
	switch {
	case err == nil:
		mon.failures = 0
		mon.suspendedAt = time.Now()
		mon.tracker.Reset()
	case errors.Is(err, machine.ErrLocked), errors.Is(err, errSuspendRunning), errors.Is(err, errSleeperStopping):
		d.log.Printf("idle suspend of %s skipped (%v); trying again on the next tick", m.Name, err)
	case errors.Is(err, backend.ErrSuspendBlocked), errors.Is(err, backend.ErrSuspendAborted):
		d.log.Printf("idle suspend of %s did not go ahead: %v", m.Name, err)
		mon.tracker.Reset()
		mon.refusal = refusalBlocker(m, err)
	default:
		mon.failures++
		mon.tracker.Reset()
		d.log.Printf("idle suspend of %s failed (%d in a row): %v", m.Name, mon.failures, err)
		if mon.failures >= idleSuspendFailures {
			mon.disabled = fmt.Sprintf("automatic suspend failed %d times in a row; it is off until the sleeper restarts ('jm stop%s' and 'jm start%s')",
				mon.failures, nameHint(m.Name), nameHint(m.Name))
			d.log.Printf("%s", mon.disabled)
		}
	}
}

// refusalBlocker is the blocker an automatic suspend's refusal leaves in
// sleeper.json: the busy share, or the reason for a blocked suspend a probe
// cannot see. Guest activity shows in the next probe, and an arriving
// client needs no explanation.
func refusalBlocker(m *machine.Machine, err error) string {
	var r *suspendRefusal
	switch {
	case errors.As(err, &r) && r.share != "":
		return "share " + r.share + " in use"
	case errors.Is(err, backend.ErrSuspendBlocked):
		msg := err.Error()
		if first, _, ok := strings.Cut(msg, "\n"); ok {
			msg = first
		}
		for _, p := range []string{m.Name + " is busy: ", "cannot suspend " + m.Name + ": "} {
			msg = strings.TrimPrefix(msg, p)
		}
		return "suspend refused: " + msg
	}
	return ""
}

// probeGuest takes the guest half of a sample on the persistent control
// connection, dialling it when there is none. A keepalive goes first; after
// idleProbeFailures failures in a row the connection is dropped and the next
// tick dials again. It never dials while a suspend runs.
func (d *sleeperDaemon) probeGuest(ctx context.Context, m *machine.Machine) (idle.Sample, error) {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.suspending.Load() {
		return idle.Sample{}, errSuspendRunning
	}
	ctx, cancel := context.WithTimeout(ctx, idleProbeTimeout)
	defer cancel()
	if d.client == nil {
		ep, err := d.p.Endpoint(m)
		if err != nil {
			return idle.Sample{}, err
		}
		c, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
		if err != nil {
			return idle.Sample{}, err
		}
		d.client, d.clientFailures = c, 0
	}
	sample, err := func() (idle.Sample, error) {
		if err := d.client.Keepalive(ctx); err != nil {
			return idle.Sample{}, err
		}
		return probeGuestActivity(ctx, d.client)
	}()
	// Only a failure of the connection counts towards dropping it: a probe
	// that ran and answered badly would fail on a fresh one too.
	var bad *probeOutputError
	if err != nil && !errors.As(err, &bad) {
		d.clientFailures++
		if d.clientFailures >= idleProbeFailures {
			_ = d.client.Close()
			d.client, d.clientFailures = nil, 0
		}
	} else {
		d.clientFailures = 0
	}
	return sample, err
}

// closeProbeClient closes the control connection, waiting for a probe in
// flight.
func (d *sleeperDaemon) closeProbeClient() {
	d.probeMu.Lock()
	defer d.probeMu.Unlock()
	if d.client != nil {
		_ = d.client.Close()
		d.client, d.clientFailures = nil, 0
	}
}

// hostSignals adds the host half of a sample: whether a jm client touched the
// activity file since the last sample, and the hypervisor's CPU use since
// then.
func (d *sleeperDaemon) hostSignals(m *machine.Machine, s *idle.Sample) {
	mon := &d.mon
	var mtime time.Time
	if fi, err := os.Stat(filepath.Join(m.Dir, machine.ActivityFile)); err == nil {
		mtime = fi.ModTime()
	}
	s.Activity = mon.activityKnown && !mtime.Equal(mon.activity)
	mon.activity, mon.activityKnown = mtime, true

	cpu, err := hypervisorCPUTime(m, d.b)
	if err != nil {
		mon.cpuKnown = false
		return
	}
	// The reading is timed when it is taken, not when the tick began: the
	// guest probe before it can take anything up to idleProbeTimeout.
	at := mon.now()
	if mon.cpuKnown && cpu >= mon.cpu {
		if dt := at.Sub(mon.cpuAt); dt > 0 {
			s.CPUPercent, s.CPUKnown = idle.CPUPercent(cpu-mon.cpu, dt), true
		}
	}
	mon.cpu, mon.cpuAt, mon.cpuKnown = cpu, at, true
}

// suspendReply renders a suspend error as a control reply: busy for guest
// activity, a busy share or a client that cancelled it, unavailable when the
// machine cannot be suspended now, err otherwise. A hint travels after a tab.
func suspendReply(err error) string {
	word := "err"
	switch {
	case errors.Is(err, backend.ErrSuspendBlocked), errors.Is(err, backend.ErrSuspendAborted):
		word = "busy"
	case errors.Is(err, backend.ErrSuspendUnavailable):
		word = "unavailable"
	}
	msg := strings.ReplaceAll(err.Error(), "\t", " ")
	var h *hintError
	if errors.As(err, &h) && h.hint != "" {
		msg += "\t" + strings.ReplaceAll(h.hint, "\t", " ")
	}
	return word + " " + msg
}

// suspendReplyError is the command side of suspendReply.
func suspendReplyError(m *machine.Machine, reply string) error {
	word, rest := sleeper.SplitReply(reply)
	msg, hint, _ := strings.Cut(rest, "\t")
	var err error
	switch word {
	case "busy":
		err = refuse(backend.ErrSuspendBlocked, "%s", msg)
	case "unavailable":
		err = refuse(backend.ErrSuspendUnavailable, "%s", msg)
	case "err":
		err = errors.New(msg)
	default:
		return fmt.Errorf("unexpected reply from the sleeper of %s: %q", m.Name, reply)
	}
	if hint != "" {
		return withHint(err, hint)
	}
	return err
}
