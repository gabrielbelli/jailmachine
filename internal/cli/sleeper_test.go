package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sleeper"
)

func TestMain(m *testing.M) {
	// The test binary is not jm: no test may launch it as a sleeper.
	launchSleeper = func(context.Context, *machine.Machine) error {
		fakeEvent("sleeper-start")
		return nil
	}
	os.Exit(m.Run())
}

// shortSocketDir is a directory short enough for unix socket paths.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jmc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// testSleeper returns the in-process sleeper of a machine while
// inProcessSleeper is in force.
var testSleeper func(m *machine.Machine) *sleeperDaemon

// inProcessSleeper answers control requests with a sleeper daemon in this
// process, one per machine, called synchronously; its mode loop never runs.
func inProcessSleeper(t *testing.T) {
	t.Helper()
	oldReq, oldTCP, oldGet := sleeperRequest, takeTCPTimeout, testSleeper
	takeTCPTimeout = 50 * time.Millisecond
	var mu sync.Mutex
	daemons := map[string]*sleeperDaemon{}
	testSleeper = func(m *machine.Machine) *sleeperDaemon {
		mu.Lock()
		defer mu.Unlock()
		d := daemons[m.Dir]
		if d == nil {
			b, p, err := components(m)
			if err != nil {
				panic(err)
			}
			d = newSleeperDaemon(m, b, p, log.New(io.Discard, "", 0))
			daemons[m.Dir] = d
		}
		return d
	}
	sleeperRequest = func(ctx context.Context, m *machine.Machine, line string) (string, error) {
		return testSleeper(m).handle(ctx, line), nil
	}
	t.Cleanup(func() {
		sleeperRequest, takeTCPTimeout, testSleeper = oldReq, oldTCP, oldGet
		for _, d := range daemons {
			d.sl.Close()
		}
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSuspendHoldsSocketAndAbortsForAClient(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedRunningForSuspend(t, root, "sl")
	d := testSleeper(m)
	var mu sync.Mutex
	var sock string
	clients := make(chan net.Conn, 1)
	startFakeGuest(t, root, "sl", func(cmd string) guestReply {
		r := defaultGuest(cmd)
		if r.label == "quiesce" {
			// A client arrives while the guest is quiesced, and is held.
			mu.Lock()
			path := sock
			mu.Unlock()
			if c, err := net.Dial("unix", path); err == nil {
				clients <- c
				// Not waitUntil: this runs on the fake sshd's goroutine.
				for deadline := time.Now().Add(5 * time.Second); d.sl.Held() == 0 && time.Now().Before(deadline); {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}
		return r
	})
	mu.Lock()
	sock = fakeNet.endpoint.APISocket
	mu.Unlock()

	_, err := run(t, root, "suspend", "sl")
	if err == nil || !strings.Contains(err.Error(), "cancelled: a client arrived") || !errors.Is(err, backend.ErrSuspendBlocked) {
		t.Fatalf("suspend = %v", err)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "commit") || !inOrder(events, "prepare", "ssh:quiesce", "api-unforward", "ssh:remount", "cancel", "api-forward") {
		t.Errorf("events = %v", events)
	}
	var c net.Conn
	select {
	case c = <-clients:
	default:
		t.Fatal("no client reached the stand-in")
	}
	defer c.Close()
	// The rollback relays it to the engine socket; nothing serves that in
	// this test, so it is closed rather than left hanging.
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("held client after the rollback: %v", err)
	}
	if d.sl.Holding() {
		t.Error("the stand-in still holds after the rollback")
	}
}

func TestSuspendTakesEndpointsInSleeper(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedRunningForSuspend(t, root, "sl")
	startFakeGuest(t, root, "sl", defaultGuest)
	errOut := captureStderr(t)

	out, err := run(t, root, "suspend", "sl")
	if err != nil {
		t.Fatalf("suspend: %v\n%s", err, out)
	}
	if !strings.Contains(out, "done: suspended sl") {
		t.Errorf("output = %q", out)
	}
	d := testSleeper(m)
	if !d.sl.UnixIsOurs() || d.sl.UnixPath() != fakeNet.endpoint.APISocket {
		t.Errorf("the stand-in does not hold the engine socket (%q)", d.sl.UnixPath())
	}
	// The fake guest's sshd holds the SSH port, so S17 cannot take it.
	if !strings.Contains(errOut.String(), "ssh:// clients will not wake sl") {
		t.Errorf("stderr = %q", errOut.String())
	}
	st, err := sleeper.LoadStatus(sleeperProcess(m).StatusPath())
	if err != nil || st.State != "suspended" || st.SuspendedAt == nil || st.SSHWake {
		t.Errorf("sleeper.json = %+v, %v", st, err)
	}
	c, err := net.Dial("unix", fakeNet.endpoint.APISocket)
	if err != nil {
		t.Fatalf("a client of the suspended machine was refused: %v", err)
	}
	defer c.Close()
	waitUntil(t, "the client to be held", func() bool { return d.sl.Held() == 1 })
}

func TestSleeperControlRequests(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedSuspended(t, root, "sl")
	d := newSleeperDaemon(m, fakeBE, fakeNet, log.New(io.Discard, "", 0))
	defer d.sl.Close()
	ctx := context.Background()
	for line, want := range map[string]string{
		"abort":           "ok",
		"release-tcp":     "err release-tcp needs a pid",
		"release-tcp x":   `err bad pid "x"`,
		"release-tcp 12":  "ok",
		"woke wrapper 48": "ok",
		"frobnicate":      "err unknown request",
	} {
		if got := d.handle(ctx, line); got != want {
			t.Errorf("%q = %q, want %q", line, got, want)
		}
	}
	st, err := sleeper.LoadStatus(sleeperProcess(m).StatusPath())
	if err != nil || st.LastWakeBy != "wrapper" || st.LastResumeMS != 48 {
		t.Errorf("sleeper.json after woke = %+v, %v", st, err)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(d.handle(ctx, "status")), &status); err != nil || status["last_wake_by"] != "wrapper" {
		t.Errorf("status = %v, %v", status, err)
	}
	d.suspending.Store(true)
	if got := d.handle(ctx, "suspend"); !strings.HasPrefix(got, "busy ") {
		t.Errorf("suspend during a suspend = %q", got)
	}
	d.suspending.Store(false)
	d.closeSuspends()
	if got := d.handle(ctx, "suspend"); !strings.HasPrefix(got, "err ") || !strings.Contains(got, "stopping") {
		t.Errorf("suspend while the sleeper stops = %q", got)
	}

	// Replies map back to errors the command can classify.
	for reply, want := range map[string]error{
		"busy sl is busy: 1 jail\t'jm suspend --force' suspends it anyway": backend.ErrSuspendBlocked,
		"unavailable cannot suspend sl: older jm":                          backend.ErrSuspendUnavailable,
	} {
		err := suspendReplyError(m, reply)
		if !errors.Is(err, want) {
			t.Errorf("%q = %v", reply, err)
		}
		if msg := formatError("suspend", "sl", err); strings.Contains(reply, "\t") && !strings.Contains(msg, "--force") {
			t.Errorf("hint lost: %s", msg)
		}
	}
	if r := suspendReply(withHint(refuse(backend.ErrSuspendAborted, "cancelled"), "retry")); r != "busy cancelled\tretry" {
		t.Errorf("suspendReply = %q", r)
	}
}

// replaceSocket binds an answering socket beside path and renames it over, as
// the engine socket forward does at a wake. The server echoes one prefix.
func replaceSocket(t *testing.T, path string) {
	t.Helper()
	tmp := path + ".new"
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				data, _ := io.ReadAll(c)
				_, _ = c.Write(append([]byte("engine:"), data...))
			}()
		}
	}()
}

// The mode loop, one evaluation at a time: a suspended machine's endpoints
// are held, a held connection spawns one waker, a waker that leaves the
// machine suspended closes the held connections and backs off, and once the
// machine runs with its real endpoints back the held connection is relayed.
func TestSleeperDaemonHoldsWakesAndHandsOver(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	oldRoot := stateRoot
	stateRoot = root
	t.Cleanup(func() { stateRoot = oldRoot })
	m := seedSuspended(t, root, "sl")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	sock := filepath.Join(shortSocketDir(t), "podman.sock")
	fakeNet.endpoint = &netprov.Endpoint{SSHHost: "127.0.0.1", SSHPort: port, APISocket: sock}

	d := newSleeperDaemon(m, fakeBE, fakeNet, log.New(io.Discard, "", 0))
	defer d.sl.Close()
	now := time.Now()
	alive, spawned := false, 0
	d.waker.Spawn = func() (int, error) { spawned++; alive = true; return 1 << 30, nil }
	d.waker.Alive = func(int) bool { return alive }
	d.waker.Now = func() time.Time { return now }

	if mode := d.evaluate(); mode != sleeper.ModeHold || !d.sl.UnixIsOurs() || !d.sl.TCPListening() {
		t.Fatalf("suspended: mode %s, unix %v, tcp %v", mode, d.sl.UnixIsOurs(), d.sl.TCPListening())
	}
	if spawned != 0 {
		t.Error("a waker was spawned with nothing held")
	}
	c1, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	waitUntil(t, "a held connection", func() bool { return d.sl.Held() == 1 })
	d.evaluate()
	d.evaluate()
	if spawned != 1 {
		t.Fatalf("spawned %d wakers for one held connection", spawned)
	}

	// The waker exits and the machine is still suspended.
	alive = false
	d.evaluate()
	_ = c1.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c1.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("held connection after a failed wake: %v", err)
	}
	c2, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	waitUntil(t, "a second held connection", func() bool { return d.sl.Held() == 1 })
	d.evaluate()
	if spawned != 1 {
		t.Error("spawned a waker during the backoff")
	}
	now = now.Add(wakeSpawnBackoff + time.Second)
	d.evaluate()
	if spawned != 2 {
		t.Fatalf("no waker after the backoff (%d)", spawned)
	}

	// The waker releases the SSH port, then brings the machine back.
	if got := d.handle(context.Background(), "release-tcp "+strconv.Itoa(os.Getpid())); got != "ok" {
		t.Fatalf("release-tcp = %q", got)
	}
	d.evaluate()
	if d.sl.TCPListening() {
		t.Fatal("the SSH port was taken back while its waker lives")
	}
	if _, err := c2.Write([]byte("GET /_ping")); err != nil {
		t.Fatal(err)
	}
	_ = c2.(*net.UnixConn).CloseWrite()
	fakeNet.state = backend.Running
	if mode := d.evaluate(); mode != sleeper.ModeHold {
		t.Errorf("mode with the network up and the guest suspended = %s", mode)
	}
	fakeBE.state = backend.Running
	if err := os.Remove(fakeJournal(m)); err != nil {
		t.Fatal(err)
	}
	if mode := d.evaluate(); mode != sleeper.ModeHold {
		t.Errorf("mode before the engine socket is back = %s", mode)
	}
	replaceSocket(t, sock)
	tcp, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	if mode := d.evaluate(); mode != sleeper.ModeMonitor {
		t.Fatalf("mode with every endpoint back = %s", mode)
	}
	reply, _ := io.ReadAll(c2)
	if string(reply) != "engine:GET /_ping" {
		t.Errorf("relayed reply = %q", reply)
	}
	if d.sl.Holding() {
		t.Error("still holding after the hand-over")
	}
	st, err := sleeper.LoadStatus(sleeperProcess(m).StatusPath())
	if err != nil || st.Mode != sleeper.ModeMonitor || st.LastWakeBy != "socket" {
		t.Errorf("sleeper.json = %+v, %v", st, err)
	}

	// Stopped twice in a row ends the sleeper, but not while a command may
	// be between states: a wake that falls back to a cold boot reads
	// stopped for a moment, with the waker alive and the lock taken.
	fakeBE.state, fakeNet.state = backend.Stopped, backend.Stopped
	if mode := d.evaluate(); mode == sleeper.ModeExit {
		t.Error("exited on the first stopped evaluation")
	}
	if mode := d.evaluate(); mode == sleeper.ModeExit {
		t.Error("exited while the waker lives")
	}
	alive = false
	unlock, err := store().Lock("sl")
	if err != nil {
		t.Fatal(err)
	}
	if mode := d.evaluate(); mode == sleeper.ModeExit {
		t.Error("exited while another command holds the lock")
	}
	unlock()
	if mode := d.evaluate(); mode != sleeper.ModeExit {
		t.Errorf("mode on a stopped evaluation with nothing in flight = %s", mode)
	}
}

// A wake that continues the guest and then fails before the engine socket
// forward takes the path back leaves a running machine behind with the
// stand-in's socket. Once no wake is in flight the sleeper lets go of it, so
// clients are refused (and wrappers start the machine's stages) instead of
// being held for a hand-over that never comes.
func TestSleeperGivesUpSocketAfterAFailedWake(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	oldRoot := stateRoot
	stateRoot = root
	t.Cleanup(func() { stateRoot = oldRoot })
	m := seedSuspended(t, root, "sl")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	sock := filepath.Join(shortSocketDir(t), "podman.sock")
	fakeNet.endpoint = &netprov.Endpoint{SSHHost: "127.0.0.1", SSHPort: port, APISocket: sock}

	d := newSleeperDaemon(m, fakeBE, fakeNet, log.New(io.Discard, "", 0))
	defer d.sl.Close()
	alive := false
	d.waker.Spawn = func() (int, error) { alive = true; return 1 << 30, nil }
	d.waker.Alive = func(int) bool { return alive }
	if mode := d.evaluate(); mode != sleeper.ModeHold || !d.sl.UnixIsOurs() {
		t.Fatalf("suspended: mode %s, unix %v", mode, d.sl.UnixIsOurs())
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitUntil(t, "a held connection", func() bool { return d.sl.Held() == 1 })
	d.evaluate()
	if !alive {
		t.Fatal("no waker for the held connection")
	}
	// The waker releases the SSH port (to a pid that is gone by now),
	// starts the network and continues the guest, and then fails.
	if got := d.handle(context.Background(), "release-tcp 1073741824"); got != "ok" {
		t.Fatalf("release-tcp = %q", got)
	}
	gvproxySSH, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer gvproxySSH.Close()
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	if err := os.Remove(fakeJournal(m)); err != nil {
		t.Fatal(err)
	}
	d.evaluate()
	if !d.sl.UnixIsOurs() {
		t.Fatal("let go of the socket while the waker lives")
	}
	alive = false
	unlock, err := store().Lock("sl")
	if err != nil {
		t.Fatal(err)
	}
	d.evaluate()
	if !d.sl.UnixIsOurs() {
		t.Fatal("let go of the socket while another command holds the lock")
	}
	unlock()

	if mode := d.evaluate(); mode != sleeper.ModeMonitor {
		t.Errorf("mode after letting go = %s", mode)
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stand-in's socket is still there: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("held connection after letting go: %v", err)
	}
	if d.sl.Holding() {
		t.Error("still holding after letting go")
	}
}

func recordSleeperStops(t *testing.T) {
	t.Helper()
	old := stopSleeperProcess
	stopSleeperProcess = func(context.Context, *machine.Machine) error {
		fakeEvent("sleeper-stop")
		return nil
	}
	t.Cleanup(func() { stopSleeperProcess = old })
}

func TestRmStopsSleeperOnBothPaths(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	isolateHostTools(t)
	captureStderr(t)
	recordSleeperStops(t)

	seedFakeRecord(t, root, "rec")
	if out, err := run(t, root, "rm", "rec"); err != nil {
		t.Fatalf("rm: %v\n%s", err, out)
	}
	if events := takeFakeEvents(); len(events) == 0 || events[0] != "sleeper-stop" {
		t.Errorf("record path: events = %v, want the sleeper stopped first", events)
	}

	dir := machine.NewStore(root).Dir("bad")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, machine.RecordFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, root, "rm", "bad"); err != nil {
		t.Fatalf("rm of a corrupt record: %v\n%s", err, out)
	}
	if events := takeFakeEvents(); !slices.Contains(events, "sleeper-stop") {
		t.Errorf("corrupt-record path: events = %v", events)
	}
}

func TestStopTidiesStaleSleeperPID(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	m := mustLoad(t, root, "sl")
	pid := filepath.Join(m.Dir, sleeper.PIDFile)
	if err := os.WriteFile(pid, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, root, "stop", "sl"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Errorf("stale sleeper.pid survived stop: %v", err)
	}
}

func TestRepairBrokenDoesNotStopSleeper(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	oldRoot, oldOut := stateRoot, stdout
	stateRoot, stdout = root, io.Discard
	t.Cleanup(func() { stateRoot, stdout = oldRoot, oldOut })
	seedFakeRecord(t, root, "sl")
	m := mustLoad(t, root, "sl")
	recordSleeperStops(t)
	pid := filepath.Join(m.Dir, sleeper.PIDFile)
	if err := os.WriteFile(pid, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBE.state, fakeNet.state = backend.Running, backend.Stopped
	if err := repairBroken(context.Background(), m, fakeBE, fakeNet, false); err != nil {
		t.Fatal(err)
	}
	if events := takeFakeEvents(); slices.Contains(events, "sleeper-stop") {
		t.Errorf("repairBroken stopped the sleeper: %v", events)
	}
	if _, err := os.Stat(pid); err != nil {
		t.Errorf("repairBroken touched sleeper.pid: %v", err)
	}
}

// A detached wake queues behind a command holding the lock and then wakes;
// starting the sleeper, which that wake does under the lock, never needs it.
func TestStartMachineReleasesNothingItDoesNotOwn(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	startFakeGuest(t, root, "sl", defaultGuest)
	isolateHostTools(t)
	captureStderr(t)

	unlock, err := machine.NewStore(root).Lock("sl")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := run(t, root, sleeper.WakeCommand, "sl")
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("_wake did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("_wake: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("_wake did not finish after the lock was released")
	}
	events := takeFakeEvents()
	if !inOrder(events, "resume", "ssh:post-resume", "api-forward", "sleeper-start") || slices.Contains(events, "boot") {
		t.Errorf("events = %v", events)
	}

	unlock, err = machine.NewStore(root).Lock("sl")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	m := mustLoad(t, root, "sl")
	started := make(chan error, 1)
	go func() { started <- startSleeper(context.Background(), m, fakeBE, fakeNet) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startSleeper waited for the machine lock")
	}
}

func TestWrapperArgsLeavesHiddenCommands(t *testing.T) {
	root := NewRootCmd()
	for _, cmd := range []string{sleeper.Command, sleeper.WakeCommand} {
		for _, wrapper := range []string{"/usr/local/bin/" + WrapperName, "/usr/local/bin/" + DockerWrapperName} {
			args := []string{wrapper, "--state-root", "/r", cmd, "dev"}
			if got := wrapperArgs(root, args); !slices.Equal(got, args) {
				t.Errorf("wrapperArgs(%q) = %q", args, got)
			}
		}
		if c, _, err := root.Find([]string{cmd}); err != nil || !c.Hidden {
			t.Errorf("%s is not a hidden command: %v", cmd, err)
		}
	}
}

func TestInspectSleeperKeys(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	m := mustLoad(t, root, "sl")
	if err := sleeper.WriteStatus(filepath.Join(m.Dir, machine.SleeperStatusFile), sleeper.Status{LastWakeBy: "socket", LastResumeMS: 4800, LastSuspendMS: 9213}); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "--json", "inspect", "sl")
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatal(err)
	}
	if keys["sleeper_state"] != "stopped" || keys["sleeper_log"] != filepath.Join(m.Dir, sleeper.LogFile) ||
		keys["last_wake_by"] != "socket" || keys["last_resume_ms"] != float64(4800) || keys["last_suspend_ms"] != float64(9213) {
		t.Errorf("inspect json = %v", keys)
	}
	for _, k := range []string{"sleeper_state", "sleeper_log", "last_wake_by", "last_resume_ms", "last_suspend_ms"} {
		if !strings.Contains(inspectLong, k) {
			t.Errorf("inspectLong lacks %q", k)
		}
	}
	if out, err := run(t, root, "inspect", "sl"); err != nil || !strings.Contains(out, "Sleeper:") {
		t.Errorf("inspect = %q, %v", out, err)
	}
}

func TestDoctorSleeperLines(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	out, _ := run(t, root, "--json", "doctor")
	var rep struct {
		Checks []struct{ Name, Status, Detail, Fix string } `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "sleeper sl" {
			found = true
			if c.Status == "ok" || !strings.Contains(c.Detail, "refused") || c.Fix != "jm start sl" {
				t.Errorf("sleeper check = %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("no sleeper check for a suspended machine without one:\n%s", out)
	}
}

func TestSetIdleSuspendStartsSleeperOnRunning(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	if out, err := run(t, root, "set", "--idle-suspend", "45", "sl"); err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if events := takeFakeEvents(); !slices.Contains(events, "sleeper-start") {
		t.Errorf("events = %v", events)
	}
	if got := mustLoad(t, root, "sl").IdleSuspendMin; got != 45 {
		t.Errorf("idle_suspend_min = %d", got)
	}
}
