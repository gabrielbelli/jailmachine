package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sleeper"
)

// idleFixture is a running fake machine with idle suspend after 5 "minutes"
// of 10 s each, a fake guest whose idle probe answers what probe holds, and
// the in-process sleeper that monitors it on a fake clock.
type idleFixture struct {
	t    *testing.T
	root string
	m    *machine.Machine
	d    *sleeperDaemon
	now  time.Time

	mu      sync.Mutex
	probe   string
	probes  int
	quiesce *guestReply
}

func newIdleFixture(t *testing.T) *idleFixture {
	t.Helper()
	resetFakes(t)
	captureStderr(t)
	root := t.TempDir()
	oldRoot, oldMinute, oldCPU := stateRoot, idleMinute, hypervisorCPUTime
	stateRoot, idleMinute = root, 10*time.Second
	hypervisorCPUTime = func(*machine.Machine, backend.Backend) (time.Duration, error) {
		return 0, errors.New("no hypervisor")
	}
	t.Cleanup(func() { stateRoot, idleMinute, hypervisorCPUTime = oldRoot, oldMinute, oldCPU })
	f := &idleFixture{t: t, root: root, probe: idleGuest}
	f.m = seedRunningForSuspend(t, root, "sl")
	f.setIdleSuspend(5)
	startFakeGuest(t, root, "sl", func(cmd string) guestReply {
		r := defaultGuest(cmd)
		if r.label == "probe" {
			f.mu.Lock()
			r.out = f.probe
			f.probes++
			f.mu.Unlock()
		}
		if r.label == "quiesce" {
			f.mu.Lock()
			if f.quiesce != nil {
				r = *f.quiesce
			}
			f.mu.Unlock()
		}
		return r
	})
	f.d = testSleeper(f.m)
	f.now = time.Now()
	f.d.mon.now = func() time.Time { return f.now }
	t.Cleanup(f.d.closeProbeClient)
	return f
}

func (f *idleFixture) setIdleSuspend(mins int) {
	f.t.Helper()
	m := mustLoad(f.t, f.root, "sl")
	m.IdleSuspendMin = mins
	if err := machine.NewStore(f.root).Save(m); err != nil {
		f.t.Fatal(err)
	}
}

func (f *idleFixture) setProbe(out string) {
	f.mu.Lock()
	f.probe = out
	f.mu.Unlock()
}

// tick advances the fake clock by one probe interval and runs monitor mode.
func (f *idleFixture) tick() {
	f.now = f.now.Add(idleProbeInterval)
	f.d.monitor(context.Background())
}

func (f *idleFixture) status() sleeper.Status {
	f.t.Helper()
	st, err := sleeper.LoadStatus(sleeperProcess(f.m).StatusPath())
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

func TestSleeperIdleMonitorSuspendsWhenIdle(t *testing.T) {
	f := newIdleFixture(t)
	d := f.d
	d.monitor(context.Background()) // enters monitor mode; first sample
	client := d.client
	if client == nil {
		t.Fatal("no control connection after the first probe")
	}
	for range 3 {
		f.tick()
	}
	st := f.status()
	if st.IdleSeconds != 45 || st.IdleSuspendAfterSeconds != 50 || st.Penalty != 1 || len(st.Blockers) != 0 || st.IdleUnavailable != "" {
		t.Fatalf("sleeper.json after 45 s idle = %+v", st)
	}
	if d.client != client {
		t.Error("the control connection was dialled again between probes")
	}
	if events := takeFakeEvents(); slices.Contains(events, "prepare") {
		t.Fatalf("suspended before the idle period: %v", events)
	}
	// A control request between ticks does not probe early.
	f.now = f.now.Add(time.Second)
	d.monitor(context.Background())
	if f.probes != 4 {
		t.Errorf("probes after an early pass = %d, want 4", f.probes)
	}
	f.now = f.now.Add(-time.Second)

	f.tick()
	events := takeFakeEvents()
	if !inOrder(events, "ssh:probe", "prepare", "ssh:quiesce", "commit", "park") {
		t.Fatalf("events = %v", events)
	}
	if fakeBE.state != backend.Suspended {
		t.Fatalf("state = %s", fakeBE.state)
	}
	if st := f.status(); st.State != "suspended" || st.LastSuspendMS < 0 {
		t.Errorf("sleeper.json after the suspend = %+v", st)
	}
	// Suspended: the sleeper holds, closes its connection and drops the
	// idle fields.
	if mode := d.evaluate(); mode != sleeper.ModeHold {
		t.Fatalf("mode after the suspend = %s", mode)
	}
	d.leaveMonitor()
	if d.client != nil {
		t.Error("the control connection outlived the suspend")
	}
	if st := f.status(); st.IdleSeconds != 0 || st.IdleSuspendAfterSeconds != 0 {
		t.Errorf("idle fields while suspended: %+v", st)
	}

	// A wake a minute later is a flap: the idle period doubles, and the
	// idle time starts from nothing.
	if got := d.handle(context.Background(), "woke wrapper 900"); got != "ok" {
		t.Fatal(got)
	}
	fakeBE.state = backend.Running
	_ = os.Remove(fakeJournal(f.m))
	d.monitor(context.Background())
	st = f.status()
	if st.Penalty != 2 || st.IdleSuspendAfterSeconds != 100 || st.IdleSeconds != 0 {
		t.Errorf("sleeper.json after a quick wake = %+v", st)
	}
}

func TestSleeperIdleMonitorBlockers(t *testing.T) {
	f := newIdleFixture(t)
	f.d.monitor(context.Background())
	f.tick()
	f.tick()
	if st := f.status(); st.IdleSeconds != 30 {
		t.Fatalf("idle = %d", st.IdleSeconds)
	}

	// A container resets at once.
	f.setProbe(busyGuest("jails", 1))
	f.tick()
	if st := f.status(); st.IdleSeconds != 0 || !slices.Equal(st.Blockers, []string{"1 jail or container running"}) {
		t.Errorf("with a container: %+v", st)
	}

	// A session must be seen twice; the forwarder's stream never counts.
	f.setProbe(busyGuest("sessions", 1))
	f.tick()
	if st := f.status(); st.IdleSeconds != 0 || len(st.Blockers) != 0 {
		t.Errorf("after one session sample: %+v", st)
	}
	f.setProbe(idleGuest)
	f.tick()
	f.setProbe(busyGuest("sessions", 1))
	f.tick()
	f.tick()
	if st := f.status(); st.IdleSeconds != 0 || !slices.Equal(st.Blockers, []string{"1 command session open"}) {
		t.Errorf("after two session samples: %+v", st)
	}

	// A jm client on the host resets.
	f.setProbe(idleGuest)
	f.tick()
	f.tick()
	bumpActivity(f.m)
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(filepath.Join(f.m.Dir, machine.ActivityFile), later, later)
	f.tick()
	if st := f.status(); st.IdleSeconds != 0 || !slices.Equal(st.Blockers, []string{"a jm command used the machine"}) || st.LastActivity == nil {
		t.Errorf("after host activity: %+v", st)
	}

	// A busy hypervisor resets: 7 s of CPU in 15 s.
	var cpu time.Duration
	hypervisorCPUTime = func(*machine.Machine, backend.Backend) (time.Duration, error) { return cpu, nil }
	f.tick()
	cpu += 7 * time.Second
	f.tick()
	if st := f.status(); st.IdleSeconds != 0 || !slices.Equal(st.Blockers, []string{"guest CPU 47%"}) || st.CPUPercent != 46.7 {
		t.Errorf("with a busy hypervisor: %+v", st)
	}
	cpu += time.Second
	f.tick()
	if st := f.status(); st.IdleSeconds != 15 {
		t.Errorf("with a calm hypervisor: %+v", st)
	}

	// An unreadable probe neither counts nor resets, and the connection
	// that carried it is kept however often it happens.
	client := f.d.client
	f.setProbe(strings.Replace(idleGuest, "sshd=1", "sshd=0", 1))
	for range idleProbeFailures + 1 {
		f.tick()
	}
	if f.d.client != client || client == nil {
		t.Error("an unusable probe answer dropped a working connection")
	}
	if st := f.status(); st.IdleSeconds != 15 || len(st.Blockers) != 1 || !strings.Contains(st.Blockers[0], "no sshd session") {
		t.Errorf("with an unreadable probe: %+v", st)
	}

	// Suspend unavailable: time counts, nothing fires.
	f.setProbe(idleGuest)
	fakeBE.suspendable = "hypervisor was started by an older jm; restart it once: jm stop && jm start"
	for range 6 {
		f.tick()
	}
	if st := f.status(); !strings.Contains(st.IdleUnavailable, "older jm") || st.IdleSeconds < 50 {
		t.Errorf("unavailable: %+v", st)
	}
	fakeBE.suspendable = ""

	// Off: the tracker resets and never fires.
	f.setIdleSuspend(0)
	for range 6 {
		f.tick()
	}
	if st := f.status(); st.IdleSeconds != 0 || st.IdleSuspendAfterSeconds != 0 {
		t.Errorf("off: %+v", st)
	}
	if events := takeFakeEvents(); slices.Contains(events, "prepare") {
		t.Errorf("a suspend started: %v", events)
	}
}

func TestSleeperIdleMonitorLockAndFailures(t *testing.T) {
	f := newIdleFixture(t)
	f.d.monitor(context.Background())

	// Another command holds the lock: skipped, idle time kept.
	unlock, err := store().Lock("sl")
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		f.tick()
	}
	unlock()
	if st := f.status(); st.IdleSeconds != 60 || st.DisabledReason != "" || st.LastSuspendError != "" {
		t.Errorf("after a skipped tick: %+v", st)
	}
	if events := takeFakeEvents(); slices.Contains(events, "prepare") {
		t.Fatalf("a suspend started under another command's lock: %v", events)
	}

	// A save that fails three times in a row turns automatic suspend off;
	// each failure backs off a full idle period.
	fakeBE.commitErr = errors.New("qemu: the save failed: No space left on device")
	for attempt := 1; attempt <= idleSuspendFailures; attempt++ {
		for n := 0; f.d.mon.failures < attempt && n < 10; n++ {
			f.tick()
		}
		if f.d.mon.failures != attempt {
			t.Fatalf("failures = %d, want %d", f.d.mon.failures, attempt)
		}
		if st := f.status(); st.IdleSeconds != 0 {
			t.Errorf("a failed suspend kept its idle time: %+v", st)
		}
	}
	st := f.status()
	if !strings.Contains(st.DisabledReason, "failed 3 times") || !strings.Contains(st.LastSuspendError, "No space left") {
		t.Fatalf("sleeper.json after 3 failures = %+v", st)
	}
	takeFakeEvents()
	for range 10 {
		f.tick()
	}
	if events := takeFakeEvents(); slices.Contains(events, "prepare") {
		t.Errorf("a disabled monitor suspended: %v", events)
	}
	if fakeBE.state != backend.Running {
		t.Errorf("state = %s", fakeBE.state)
	}
}

// The real mode loop, on the real clock with millisecond intervals, suspends
// an idle machine and keeps holding it.
func TestSleeperLoopSuspendsIdleMachine(t *testing.T) {
	f := newIdleFixture(t)
	oldInterval := idleProbeInterval
	idleProbeInterval, idleMinute = 20*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { idleProbeInterval = oldInterval })
	f.d.mon.now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.d.loop(ctx)
	}()
	waitUntil(t, "the idle machine to be suspended", func() bool {
		st, err := sleeper.LoadStatus(sleeperProcess(f.m).StatusPath())
		return err == nil && st.State == "suspended" && st.Mode == sleeper.ModeHold
	})
	cancel()
	<-done
	if events := takeFakeEvents(); !inOrder(events, "prepare", "commit", "park") {
		t.Errorf("events = %v", events)
	}
	if f.d.client != nil {
		t.Error("the control connection outlived the suspend")
	}
}

func TestWrappersAndSSHBumpActivity(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	m := mustLoad(t, root, "sl")
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	path := filepath.Join(m.Dir, machine.ActivityFile)

	oldRoot := stateRoot
	stateRoot = root
	t.Cleanup(func() { stateRoot = oldRoot })
	if err := ensureRunning(context.Background(), "sl", true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("a wrapper did not bump the activity file: %v", err)
	}
	past := fi.ModTime().Add(-time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	old := sshInteractive
	sshInteractive = func(string, int, string, string, []string) error { return nil }
	t.Cleanup(func() { sshInteractive = old })
	if out, err := run(t, root, "ssh", "sl", "--", "true"); err != nil {
		t.Fatalf("ssh: %v\n%s", err, out)
	}
	if fi, err := os.Stat(path); err != nil || !fi.ModTime().After(past.Add(30*time.Minute)) {
		t.Errorf("jm ssh did not bump the activity file: %v", err)
	}
}

func TestInspectAndDoctorIdleLines(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	m := mustLoad(t, root, "sl")
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	plentyOfSpace(t)
	old := sleeperAlive
	sleeperAlive = func(*machine.Machine) bool { return true }
	t.Cleanup(func() { sleeperAlive = old })
	statusPath := filepath.Join(m.Dir, machine.SleeperStatusFile)
	write := func(st sleeper.Status) {
		t.Helper()
		st.Mode = sleeper.ModeMonitor
		if err := sleeper.WriteStatus(statusPath, st); err != nil {
			t.Fatal(err)
		}
	}
	doctorLine := func() (found bool, status, detail, fix string) {
		t.Helper()
		out, _ := run(t, root, "--json", "doctor")
		var rep struct {
			Checks []struct{ Name, Status, Detail, Fix string } `json:"checks"`
		}
		if err := json.Unmarshal([]byte(out), &rep); err != nil {
			t.Fatalf("bad json: %v\n%s", err, out)
		}
		for _, c := range rep.Checks {
			if c.Name == "idle suspend sl" {
				return true, c.Status, c.Detail, c.Fix
			}
		}
		return false, "", "", ""
	}

	write(sleeper.Status{IdleSeconds: 720, IdleSuspendAfterSeconds: 1800, Penalty: 1})
	out, err := run(t, root, "--json", "inspect", "sl")
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatal(err)
	}
	if keys["idle_seconds"] != float64(720) || keys["idle_suspend_after_seconds"] != float64(1800) {
		t.Errorf("inspect json = %v", keys)
	}
	for _, k := range []string{"idle_seconds", "idle_suspend_after_seconds", "idle_blockers", "idle_unavailable", "idle_disabled_reason"} {
		if !strings.Contains(inspectLong, k) {
			t.Errorf("inspectLong lacks %q", k)
		}
	}
	if out, _ := run(t, root, "inspect", "sl"); !strings.Contains(out, "after 30 min (idle 12 min)") {
		t.Errorf("inspect = %q", out)
	}
	if found, status, detail, _ := doctorLine(); !found || status != "ok" || !strings.Contains(detail, "idle 12 min") {
		t.Errorf("doctor idle line = %v %s %q", found, status, detail)
	}

	write(sleeper.Status{IdleSuspendAfterSeconds: 1800, Blockers: []string{"1 engine client connected"}})
	if out, _ := run(t, root, "inspect", "sl"); !strings.Contains(out, "held awake by: 1 engine client connected") {
		t.Errorf("inspect = %q", out)
	}
	if found, status, detail, _ := doctorLine(); !found || status != "ok" || !strings.Contains(detail, "held awake by: 1 engine client connected") {
		t.Errorf("doctor idle line = %v %s %q", found, status, detail)
	}

	write(sleeper.Status{IdleUnavailable: "need 6.0 GiB free"})
	if out, _ := run(t, root, "inspect", "sl"); !strings.Contains(out, "unavailable (need 6.0 GiB free)") {
		t.Errorf("inspect = %q", out)
	}
	if found, _, _, _ := doctorLine(); found {
		t.Error("doctor repeats the unavailable reason suspend readiness reports")
	}

	write(sleeper.Status{DisabledReason: "automatic suspend failed 3 times in a row", LastSuspendError: "no space"})
	if found, status, detail, fix := doctorLine(); !found || status != "warn" || !strings.Contains(detail, "no space") || !strings.Contains(fix, "jm stop sl") {
		t.Errorf("doctor idle line = %v %s %q %q", found, status, detail, fix)
	}

	// Not monitoring, or suspend off.
	m.IdleSuspendMin = 0
	if err := machine.NewStore(root).Save(m); err != nil {
		t.Fatal(err)
	}
	if found, status, detail, _ := doctorLine(); !found || status != "ok" || !strings.Contains(detail, "off") {
		t.Errorf("doctor idle line when off = %v %s %q", found, status, detail)
	}
	sleeperAlive = func(*machine.Machine) bool { return false }
	out, _ = run(t, root, "--json", "inspect", "sl")
	if strings.Contains(out, "idle_seconds") {
		t.Errorf("idle keys without a live sleeper:\n%s", out)
	}
	if found, _, _, _ := doctorLine(); found {
		t.Error("doctor idle line without a live sleeper")
	}
}

// A share the guest will not unmount refuses the automatic suspend; no probe
// can see why, so the refusal stays among the blockers while the idle time
// counts again, until the next attempt.
func TestSleeperIdleMonitorBusyShare(t *testing.T) {
	f := newIdleFixture(t)
	f.mu.Lock()
	f.quiesce = &guestReply{"quiesce", "busy /Users/me/src\n", quiesceBusy}
	f.mu.Unlock()
	f.d.monitor(context.Background())
	for n := 0; n < 10 && !slices.Contains(f.status().Blockers, "share /Users/me/src in use"); n++ {
		f.tick()
	}
	st := f.status()
	if !slices.Equal(st.Blockers, []string{"share /Users/me/src in use"}) || st.IdleSeconds != 0 || fakeBE.state != backend.Running {
		t.Fatalf("after a busy share: %+v (state %s)", st, fakeBE.state)
	}
	if f.d.mon.failures != 0 {
		t.Errorf("a busy share counted as a failure: %d", f.d.mon.failures)
	}
	f.tick()
	if st := f.status(); st.IdleSeconds != 15 || !slices.Equal(st.Blockers, []string{"share /Users/me/src in use"}) {
		t.Errorf("the refusal did not stay while idle counts: %+v", st)
	}
	// Freed: the next attempt clears it and suspends.
	f.mu.Lock()
	f.quiesce = nil
	f.mu.Unlock()
	for n := 0; n < 10 && fakeBE.state != backend.Suspended; n++ {
		f.tick()
	}
	if fakeBE.state != backend.Suspended || f.d.mon.refusal != "" {
		t.Errorf("after the share was freed: state %s, refusal %q", fakeBE.state, f.d.mon.refusal)
	}
}

// The hypervisor's CPU time is divided by the time between the two
// readings, not between the ticks that took them: a slow guest probe before
// one reading does not skew it.
func TestSleeperIdleCPUTimedAtReading(t *testing.T) {
	f := newIdleFixture(t)
	var cpu time.Duration
	slow := true
	hypervisorCPUTime = func(*machine.Machine, backend.Backend) (time.Duration, error) {
		if slow {
			// The probe before this reading took 10 s.
			f.now = f.now.Add(10 * time.Second)
		}
		return cpu, nil
	}
	f.d.monitor(context.Background())
	f.tick()
	// The next tick starts 25 s after this one did, but its reading comes
	// 15 s after this one's.
	slow = false
	cpu += 6 * time.Second
	f.tick()
	if st := f.status(); st.CPUPercent != 40 {
		t.Errorf("cpu_percent = %v, want 40 (6 s in 15 s)", st.CPUPercent)
	}
}
