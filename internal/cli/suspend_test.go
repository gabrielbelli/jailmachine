package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// resetFakes puts the fake backend and provider back to a stopped machine
// with no scripting, now and when the test ends.
func resetFakes(t *testing.T) {
	t.Helper()
	reset := func() {
		*fakeBE = fakeBackend{state: backend.Stopped}
		*fakeNet = fakeProvider{state: backend.Stopped}
		takeFakeEvents()
	}
	reset()
	t.Cleanup(reset)
}

// captureStderr redirects the CLI's stderr into a buffer for the test.
func captureStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	old := stderr
	stderr = &b
	t.Cleanup(func() { stderr = old })
	return &b
}

// isolateHostTools replaces PATH with a directory holding fake podman and
// ssh-keygen programs that log their arguments, so no test touches the real
// podman connections or ~/.ssh/known_hosts. It returns the log.
func isolateHostTools(t *testing.T) func() string {
	t.Helper()
	bin := t.TempDir()
	logPath := filepath.Join(bin, "calls.log")
	for _, name := range []string{"podman", "ssh-keygen"} {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> '" + logPath + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return func() string {
		data, _ := os.ReadFile(logPath)
		return string(data)
	}
}

// plentyOfSpace makes the free-space preflight pass.
func plentyOfSpace(t *testing.T) {
	t.Helper()
	old := freeBytes
	freeBytes = func(string) (uint64, error) { return 1 << 50, nil }
	t.Cleanup(func() { freeBytes = old })
}

// guestReply scripts one command of the fake guest: a label for the event
// log, standard output and exit status.
type guestReply struct {
	label string
	out   string
	code  int
}

// defaultGuest answers every command jm sends a provisioned, idle guest.
func defaultGuest(cmd string) guestReply {
	switch {
	case strings.Contains(cmd, "printf 'jails="):
		return guestReply{"probe", "jails=0\nengine=0\nsessions=0\ninhibit=0\n", 0}
	case strings.Contains(cmd, "umount"):
		return guestReply{"quiesce", "ok\n", 0}
	case strings.Contains(cmd, "date -u -f %s"):
		return guestReply{"post-resume", "", 0}
	case strings.Contains(cmd, "jm_remount"):
		return guestReply{"remount", "", 0}
	case cmd == "poweroff":
		return guestReply{"poweroff", "", 0}
	case strings.Contains(cmd, machine.GuestProvisionFailed):
		return guestReply{"provision", "", 1}
	case strings.HasPrefix(cmd, "test "):
		return guestReply{"test", "", 0}
	}
	return guestReply{"other", "", 0}
}

// startFakeGuest serves an in-process sshd for machine name that accepts its
// key and answers commands with reply, recording "ssh:<label>" events and
// the commands. The fake provider's endpoint is pointed at it, with an API
// socket path so the connect stage runs.
func startFakeGuest(t *testing.T, root, name string, reply func(string) guestReply) func() []string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := machine.NewStore(root).Path(name, machine.SSHKeyFile)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	clientKey, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), clientKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var mu sync.Mutex
	var cmds []string
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, creqs, err := nc.Accept()
					if err != nil {
						return
					}
					go func() {
						for r := range creqs {
							if r.Type != "exec" {
								r.Reply(false, nil)
								continue
							}
							var p struct{ Cmd string }
							_ = ssh.Unmarshal(r.Payload, &p)
							mu.Lock()
							cmds = append(cmds, p.Cmd)
							mu.Unlock()
							rep := reply(p.Cmd)
							fakeEvent("ssh:" + rep.label)
							r.Reply(true, nil)
							_, _ = ch.Write([]byte(rep.out))
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(rep.code)}))
							ch.Close()
							return
						}
					}()
				}
			}()
		}
	}()
	fakeNet.endpoint = &netprov.Endpoint{
		SSHHost: "127.0.0.1", SSHPort: ln.Addr().(*net.TCPAddr).Port,
		APISocket: filepath.Join(shortSocketDir(t), "podman-"+name+".sock"),
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), cmds...)
	}
}

// seedSuspended seeds a fake machine that is suspended with a saved state.
func seedSuspended(t *testing.T, root, name string) *machine.Machine {
	t.Helper()
	seedFakeRecord(t, root, name)
	m := mustLoad(t, root, name)
	if err := os.WriteFile(fakeJournal(m), []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBE.state, fakeNet.state = backend.Suspended, backend.Stopped
	fakeBE.meta = map[string]string{"arc_mib": strconv.Itoa(m.ArcMiB), "resolver_port": "0"}
	fakeBE.savedAt = time.Date(2026, 9, 14, 14, 2, 0, 0, time.UTC)
	fakeBE.allocated = 1395864371
	return m
}

// sshEvents keeps the guest commands from an event list.
func sshEvents(events []string) []string {
	var out []string
	for _, e := range events {
		if strings.HasPrefix(e, "ssh:") {
			out = append(out, e)
		}
	}
	return out
}

// inOrder reports whether want appears in events in that order.
func inOrder(events []string, want ...string) bool {
	i := 0
	for _, e := range events {
		if i < len(want) && e == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestCombineStateSuspendedRows(t *testing.T) {
	cases := []struct {
		bs, ps     backend.State
		supervised bool
		want       backend.State
	}{
		{backend.Suspended, backend.Stopped, true, backend.Suspended},
		{backend.Suspended, backend.Running, true, backend.Suspended},
		{backend.Suspended, backend.Running, false, backend.Suspended},
		{backend.Suspended, backend.Broken, true, backend.Broken},
		{backend.Broken, backend.Stopped, true, backend.Broken},
		{backend.Running, backend.Stopped, true, backend.Broken},
	}
	for _, c := range cases {
		if got := combineState(c.bs, c.ps, c.supervised); got != c.want {
			t.Errorf("combine(%s, %s, %v) = %s, want %s", c.bs, c.ps, c.supervised, got, c.want)
		}
	}
}

func TestStartOnSuspendedWakesWithoutBootOrConnectionSubprocesses(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedSuspended(t, root, "sl")
	cmds := startFakeGuest(t, root, "sl", defaultGuest)
	calls := isolateHostTools(t)

	out, err := run(t, root, "start", "sl")
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "boot") || !slices.Contains(events, "resume") {
		t.Errorf("a wake must resume, not boot: %v", events)
	}
	if got := sshEvents(events); !slices.Equal(got, []string{"ssh:post-resume"}) {
		t.Errorf("guest commands = %v, want the post-resume script alone", got)
	}
	if !inOrder(events, "net-start", "resume", "ssh:post-resume", "api-forward") {
		t.Errorf("wake order = %v", events)
	}
	if c := calls(); c != "" {
		t.Errorf("a wake ran host tools (podman connections, ssh-keygen): %q", c)
	}
	if got := cmds(); len(got) != 1 || !strings.Contains(got[0], "date -u -f %s ") {
		t.Errorf("post-resume script does not step the clock: %q", got)
	}
	if _, err := os.Stat(filepath.Join(m.Dir, machine.ActivityFile)); err != nil {
		t.Errorf("a wake should mark activity: %v", err)
	}
	if !strings.Contains(out, "woke sl") {
		t.Errorf("output = %q", out)
	}
}

func TestStartOnSuspendedIncompatibleFallsBackToColdBoot(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	fakeBE.resumeErr = fmt.Errorf("%w: disk.raw changed after the state was saved", backend.ErrResumeIncompatible)
	startFakeGuest(t, root, "sl", defaultGuest)
	calls := isolateHostTools(t)
	errOut := captureStderr(t)

	out, err := run(t, root, "start", "sl")
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if !inOrder(events, "resume", "discard", "park", "net-start", "boot") {
		t.Errorf("events = %v, want resume, discard, then a cold boot", events)
	}
	if !strings.Contains(errOut.String(), "could not restore the suspended state of sl (disk.raw changed") ||
		!strings.Contains(errOut.String(), "booting from disk") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if !strings.Contains(calls(), "podman system connection add") {
		t.Errorf("the cold boot should connect podman: %q", calls())
	}
}

func TestStartOnSuspendedTransientStaysSuspended(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedSuspended(t, root, "sl")
	fakeBE.resumeErr = errors.New("qemu: exited before its monitor answered")
	startFakeGuest(t, root, "sl", defaultGuest)
	isolateHostTools(t)

	_, err := run(t, root, "start", "sl")
	if err == nil || !strings.Contains(err.Error(), "could not wake sl") {
		t.Fatalf("start = %v", err)
	}
	if msg := formatError("start", "sl", err); !strings.Contains(msg, "jm stop --force sl") {
		t.Errorf("error lacks the discard hint: %s", msg)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "discard") || slices.Contains(events, "boot") || !slices.Contains(events, "park") {
		t.Errorf("a transient failure must park the network and keep the state: %v", events)
	}
	if fakeBE.state != backend.Suspended {
		t.Errorf("backend state = %s", fakeBE.state)
	}
	if _, err := os.Stat(fakeJournal(m)); err != nil {
		t.Errorf("journal gone after a transient failure: %v", err)
	}
}

func TestWakeDoesNotFoldEnvIntoRecord(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	before := seedSuspended(t, root, "sl")
	startFakeGuest(t, root, "sl", defaultGuest)
	isolateHostTools(t)
	t.Setenv(forwarder.PublishAddrEnv, "127.0.0.1")
	fakeNet.mtu = 9000

	if out, err := run(t, root, "start", "sl"); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	after := mustLoad(t, root, "sl")
	if after.PublishAddr != before.PublishAddr || after.MTU != before.MTU {
		t.Errorf("wake changed the record: publish %q -> %q, mtu %d -> %d", before.PublishAddr, after.PublishAddr, before.MTU, after.MTU)
	}
}

func TestStartRecomputesStateAfterRepair(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	fakeNet.state = backend.Broken // a stale provider beside a saved state
	startFakeGuest(t, root, "sl", defaultGuest)
	isolateHostTools(t)

	if out, err := run(t, root, "start", "sl"); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if !inOrder(events, "net-stop", "net-start", "resume") || slices.Contains(events, "boot") {
		t.Errorf("broken -> repair -> wake expected, got %v", events)
	}
}

func TestRunningStartConsumesPendingRemount(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "run")
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	cmds := startFakeGuest(t, root, "run", defaultGuest)
	isolateHostTools(t)

	if out, err := run(t, root, "start", "run"); err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}
	found := false
	for _, c := range cmds() {
		if strings.Contains(c, "jm_remount && rm -f "+machine.GuestSuspendMounts) {
			found = true
		}
	}
	if !found {
		t.Errorf("a running start must mount shares a suspend left unmounted; guest ran %d commands", len(cmds()))
	}
}

// suspendGuest is defaultGuest with the quiesce step's reply replaced.
func suspendGuest(probe, quiesce *guestReply, seen *[]string) func(string) guestReply {
	var mu sync.Mutex
	return func(cmd string) guestReply {
		r := defaultGuest(cmd)
		mu.Lock()
		defer mu.Unlock()
		switch r.label {
		case "probe":
			if probe != nil {
				return *probe
			}
		case "quiesce":
			if seen != nil {
				*seen = append(*seen, cmd)
			}
			if quiesce != nil {
				return *quiesce
			}
		}
		return r
	}
}

func seedRunningForSuspend(t *testing.T, root, name string) *machine.Machine {
	t.Helper()
	seedFakeRecord(t, root, name)
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	plentyOfSpace(t)
	inProcessSleeper(t)
	return mustLoad(t, root, name)
}

func TestSuspendRollbackOnBusyShare(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedRunningForSuspend(t, root, "sl")
	startFakeGuest(t, root, "sl", suspendGuest(nil, &guestReply{"quiesce", "busy /Users/me/src\n", quiesceBusy}, nil))
	// The engine socket forward serves podman.sock while the suspend starts.
	forward, err := net.Listen("unix", fakeNet.endpoint.APISocket)
	if err != nil {
		t.Fatal(err)
	}
	defer forward.Close()
	go func() {
		for {
			c, err := forward.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("forward"))
			c.Close()
		}
	}()
	inode := func(path string) uint64 {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return st.Sys().(*syscall.Stat_t).Ino
	}
	forwardIno := inode(fakeNet.endpoint.APISocket)

	_, err = run(t, root, "suspend", "sl")
	if err == nil || !strings.Contains(err.Error(), "shared directory is in use") || !strings.Contains(err.Error(), "/Users/me/src") {
		t.Fatalf("suspend = %v", err)
	}
	if !errors.Is(err, backend.ErrSuspendBlocked) {
		t.Errorf("a busy share should be a blocked suspend: %v", err)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "commit") {
		t.Errorf("the guest must never be frozen: %v", events)
	}
	// The forward is stopped only after the quiesce (S8); the stand-in holds
	// the socket from before it (S6).
	if slices.Contains(events, "api-unforward") || !inOrder(events, "prepare", "ssh:quiesce", "ssh:remount", "cancel", "api-forward") {
		t.Errorf("rollback order = %v", events)
	}
	if _, err := os.Stat(fakeJournal(m)); !os.IsNotExist(err) {
		t.Errorf("journal survived the rollback: %v", err)
	}
	if fakeBE.state != backend.Running {
		t.Errorf("state = %s", fakeBE.state)
	}
	// A rollback before S8 gives the running forward its socket back rather
	// than cutting the clients streaming through it.
	if got := inode(fakeNet.endpoint.APISocket); got != forwardIno {
		t.Error("the rollback did not put the forward's socket back")
	}
	c, err := net.Dial("unix", fakeNet.endpoint.APISocket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, _ := io.ReadAll(c); string(got) != "forward" {
		t.Errorf("podman.sock after the rollback answered %q", got)
	}
	if _, err := os.Lstat(filepath.Dir(fakeNet.endpoint.APISocket)); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(fakeNet.endpoint.APISocket)); len(entries) != 1 {
		t.Errorf("litter beside podman.sock: %v", entries)
	}
}

// A wrapper that finds the journal saving sends abort. One that arrives
// before the stand-in holds the engine socket still cancels the suspend, and
// before the guest is quiesced.
func TestSuspendAbortBeforeStandinCancels(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedRunningForSuspend(t, root, "sl")
	startFakeGuest(t, root, "sl", defaultGuest)
	fakeBE.onPrepare = func(pm *machine.Machine) { abortSuspendInFlight(context.Background(), pm) }

	_, err := run(t, root, "suspend", "sl")
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("suspend = %v", err)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "commit") || slices.Contains(events, "ssh:quiesce") || !inOrder(events, "prepare", "cancel", "api-forward") {
		t.Errorf("events = %v", events)
	}
	if _, err := os.Stat(fakeJournal(m)); !os.IsNotExist(err) {
		t.Errorf("journal survived the abort: %v", err)
	}
}

func TestSuspendRollbackOnActive(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedRunningForSuspend(t, root, "sl")

	// Found by the probe under the lock: nothing changes at all.
	startFakeGuest(t, root, "sl", suspendGuest(&guestReply{"probe", "jails=1\nengine=0\nsessions=0\ninhibit=0\n", 0}, nil, nil))
	_, err := run(t, root, "suspend", "sl")
	if err == nil || !strings.Contains(err.Error(), "busy: 1 jail or container running") {
		t.Fatalf("suspend with a container = %v", err)
	}
	if msg := formatError("suspend", "sl", err); !strings.Contains(msg, "--force") {
		t.Errorf("error lacks the --force hint: %s", msg)
	}
	if events := takeFakeEvents(); slices.Contains(events, "prepare") {
		t.Errorf("a busy guest must not get a journal: %v", events)
	}

	// Found by the quiesce script: rolled back.
	startFakeGuest(t, root, "sl", suspendGuest(nil, &guestReply{"quiesce", "active jails=0 engine=1 sessions=0\n", quiesceActive}, nil))
	_, err = run(t, root, "suspend", "sl")
	if err == nil || !strings.Contains(err.Error(), "engine=1") {
		t.Fatalf("suspend with an engine client = %v", err)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "commit") || !inOrder(events, "prepare", "ssh:quiesce", "cancel", "api-forward") {
		t.Errorf("events = %v", events)
	}
}

func TestSuspendForceAndSuccess(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedRunningForSuspend(t, root, "sl")
	var quiesced []string
	startFakeGuest(t, root, "sl", suspendGuest(&guestReply{"probe", "jails=2\nengine=0\nsessions=0\ninhibit=0\n", 0}, nil, &quiesced))

	out, err := run(t, root, "suspend", "--force", "sl")
	if err != nil {
		t.Fatalf("suspend --force: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if !inOrder(events, "prepare", "ssh:quiesce", "api-unforward", "commit", "park") {
		t.Errorf("events = %v", events)
	}
	if len(quiesced) != 1 || !strings.Contains(quiesced[0], "JM_FORCE=1") {
		t.Errorf("--force must reach the quiesce script: %q", quiesced)
	}
	if fakeBE.meta["containers"] != "2" || fakeBE.meta["arc_mib"] != strconv.Itoa(m.ArcMiB) {
		t.Errorf("journal meta = %v", fakeBE.meta)
	}
	if !strings.Contains(out, "suspended sl") {
		t.Errorf("output = %q", out)
	}

	// Suspending a suspended machine is not an error.
	out, err = run(t, root, "suspend", "sl")
	if err != nil || !strings.Contains(out, "already suspended") {
		t.Errorf("second suspend = %q, %v", out, err)
	}
	// Nor is it for a stopped one, which is refused.
	fakeBE.state, fakeNet.state = backend.Stopped, backend.Stopped
	_ = os.Remove(fakeJournal(m))
	if _, err := run(t, root, "suspend", "sl"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("suspend on stopped = %v", err)
	}
}

func TestSuspendCommitFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantMsg   string
		wantEvent string
		wantState backend.State
	}{
		{"save failed", errors.New("qemu: the save failed: No space left on device"), "running again", "cancel", backend.Running},
		{"blocked", fmt.Errorf("%w: blocked: a device", backend.ErrSuspendBlocked), "refuses to save", "cancel", backend.Running},
		{"hypervisor died", fmt.Errorf("%w: pid 1 is gone", backend.ErrSuspendCrashed), "hypervisor exited while suspending", "discard", backend.Running},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFakes(t)
			root := t.TempDir()
			seedRunningForSuspend(t, root, "sl")
			startFakeGuest(t, root, "sl", defaultGuest)
			fakeBE.commitErr = tc.err
			_, err := run(t, root, "suspend", "sl")
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("suspend = %v, want %q", err, tc.wantMsg)
			}
			events := takeFakeEvents()
			if !inOrder(events, "commit", tc.wantEvent) {
				t.Errorf("events = %v, want %s after commit", events, tc.wantEvent)
			}
			if tc.wantEvent == "discard" && !inOrder(events, "commit", "discard", "park") {
				t.Errorf("a dead hypervisor must discard and park: %v", events)
			}
		})
	}
}

func TestSuspendRefusesUnavailable(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedRunningForSuspend(t, root, "sl")
	fakeBE.suspendable = "hypervisor was started by an older jm; restart it once: jm stop && jm start"
	_, err := run(t, root, "suspend", "sl")
	if err == nil || !errors.Is(err, backend.ErrSuspendUnavailable) || !strings.Contains(err.Error(), "older jm") {
		t.Errorf("suspend on a legacy hypervisor = %v", err)
	}
	fakeBE.suspendable = ""
	freeBytes = func(string) (uint64, error) { return 1 << 30, nil }
	_, err = run(t, root, "suspend", "sl")
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Errorf("suspend without space = %v", err)
	}
	if events := takeFakeEvents(); slices.Contains(events, "prepare") {
		t.Errorf("a preflight refusal changed something: %v", events)
	}
}

func TestQuiesceScriptGolden(t *testing.T) {
	for _, force := range []bool{false, true} {
		s := quiesceScript(force)
		want := []string{
			"set -u; cd /",
			fmt.Sprintf("JM_FORCE=%d", map[bool]int{false: 0, true: 1}[force]),
			"sync; zpool sync 2>/dev/null; sync",
			"mount -p -t p9fs > " + machine.GuestSuspendMounts,
			"tail -r " + machine.GuestSuspendMounts,
			`d=$(printf '%b' "$dir")`,
			`umount "$d" || { echo "busy $d"; exit 4; }`,
			"done || { jm_remount; exit 4; }",
			"ps -ax -o pid= -o ppid= -o comm=",
			"[ ! -e " + machine.GuestNoSleep + " ]",
			"echo ok",
		}
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("force=%v: script lacks %q", force, w)
			}
		}
		for _, bad := range []string{"umount -f", "jm_shares stop", "pid=,"} {
			if strings.Contains(s, bad) {
				t.Errorf("force=%v: script contains %q", force, bad)
			}
		}
		if strings.Index(s, "zpool sync") > strings.Index(s, "tail -r") {
			t.Errorf("force=%v: file systems must be flushed before the unmounts", force)
		}
		if out, err := exec.Command("/bin/sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("force=%v: sh -n: %v: %s", force, err, out)
		}
	}
	// mount -p escapes a space in a path as \040; printf %b decodes it.
	out, err := exec.Command("/bin/sh", "-c", `dir='/Users/me/My\040Files'; printf '%b' "$dir"`).Output()
	if err != nil || string(out) != "/Users/me/My Files" {
		t.Errorf("printf %%b = %q, %v", out, err)
	}
	for _, s := range []string{remountScript(), activityProbe} {
		if out, err := exec.Command("/bin/sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v: %s\n%s", err, out, s)
		}
	}
	if strings.Contains(jmRemountFn, "umount") || !strings.Contains(jmRemountFn, "service jm_shares start") {
		t.Errorf("jm_remount must use the boot-time service and never unmount:\n%s", jmRemountFn)
	}
}

func TestParseGuestActivity(t *testing.T) {
	a, err := parseGuestActivity("jails=0\nengine=1\nsessions=2\ninhibit=1\n")
	if err != nil || a.jails != 0 || a.engine != 1 || a.sessions != 2 || !a.inhibit {
		t.Fatalf("parse = %+v, %v", a, err)
	}
	if got := a.blockers(1); !slices.Equal(got, []string{"2 command sessions open", machine.GuestNoSleep + " exists"}) {
		t.Errorf("blockers with the forwarder's stream = %q", got)
	}
	if got := a.blockers(0); len(got) != 3 || got[0] != "1 engine client connected" {
		t.Errorf("blockers without it = %q", got)
	}
	for _, bad := range []string{"", "jails=0\nengine=0\nsessions=0\n", "jails=x\nengine=0\nsessions=0\ninhibit=0\n"} {
		if _, err := parseGuestActivity(bad); err == nil {
			t.Errorf("parse(%q) should fail", bad)
		}
	}
}

func TestPostResumeScriptAlwaysStepsClock(t *testing.T) {
	for _, tc := range []struct{ arc, dns string }{{"", ""}, {"echo arc", ""}, {"", "echo 'dns'"}, {"exit 3", "exit 4"}} {
		s := postResumeScript(1726300000, tc.arc, tc.dns)
		if !strings.HasPrefix(s, "date -u -f %s 1726300000 >/dev/null") {
			t.Errorf("arc=%q dns=%q: the clock is not stepped first:\n%s", tc.arc, tc.dns, s)
		}
		if !strings.Contains(s, "jm_remount; then rm -f "+machine.GuestSuspendMounts) {
			t.Errorf("no share remount:\n%s", s)
		}
		want := 0
		for _, prog := range []string{tc.arc, tc.dns} {
			if prog != "" {
				want++
				if !strings.Contains(s, "/bin/sh -c "+shellQuote(prog)) {
					t.Errorf("%q is not embedded:\n%s", prog, s)
				}
			}
		}
		if got := strings.Count(s, "/bin/sh -c "); got != want {
			t.Errorf("arc=%q dns=%q: programs not embedded as asked:\n%s", tc.arc, tc.dns, s)
		}
		if !strings.Contains(s, "test -S "+machine.GuestPodmanSocket) || !strings.HasSuffix(s, "exit 0\n") {
			t.Errorf("socket check or exit missing:\n%s", s)
		}
		if out, err := exec.Command("/bin/sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v: %s", err, out)
		}
	}
	// An embedded program that exits does not end the script early.
	s := strings.Replace(postResumeScript(0, "exit 3", ""), "date -u -f %s 0 >/dev/null", "true", 1)
	s = strings.ReplaceAll(s, "test -S "+machine.GuestPodmanSocket, "true")
	s = strings.Replace(s, "if [ -f "+machine.GuestSuspendMounts+" ]", "if false", 1)
	out, err := exec.Command("/bin/sh", "-c", s).Output()
	if err != nil || strings.TrimSpace(string(out)) != "warn: the ZFS ARC cap was not applied (retried on the next start)" {
		t.Errorf("run = %q, %v", out, err)
	}
}

func TestStopOnSuspendedRestoresThenPowersOff(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	startFakeGuest(t, root, "sl", defaultGuest)

	out, err := run(t, root, "stop", "sl")
	if err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if !inOrder(events, "net-start", "resume", "ssh:poweroff", "stop graceful=true", "net-stop") {
		t.Errorf("events = %v", events)
	}
	if slices.Contains(events, "discard") || slices.Contains(events, "api-forward") || slices.Contains(events, "ssh:post-resume") {
		t.Errorf("a restore for shutdown reconnects nothing and discards nothing: %v", events)
	}
	if fakeBE.state != backend.Stopped || fakeNet.state != backend.Stopped {
		t.Errorf("states = %s/%s", fakeBE.state, fakeNet.state)
	}
}

func TestStopForceOnSuspendedDiscards(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	out, err := run(t, root, "stop", "--force", "sl")
	if err != nil {
		t.Fatalf("stop --force: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "resume") || !inOrder(events, "discard", "net-stop") {
		t.Errorf("events = %v", events)
	}
	if out, _ := run(t, root, "list"); !strings.Contains(out, "stopped") {
		t.Errorf("list = %q", out)
	}
}

func TestStopOnSuspendedTransientKeepsState(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	fakeBE.resumeErr = errors.New("qemu: binary missing")
	_, err := run(t, root, "stop", "sl")
	if err == nil || !strings.Contains(formatError("stop", "sl", err), "jm stop --force sl") {
		t.Fatalf("stop = %v", err)
	}
	if fakeBE.state != backend.Suspended || slices.Contains(takeFakeEvents(), "discard") {
		t.Errorf("a failed restore must leave the machine suspended")
	}
}

func TestRmOnSuspendedDiscardsWithoutResume(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedSuspended(t, root, "sl")
	isolateHostTools(t)
	if out, err := run(t, root, "rm", "sl"); err != nil {
		t.Fatalf("rm: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if slices.Contains(events, "resume") || !slices.Contains(events, "discard") {
		t.Errorf("events = %v", events)
	}
	if _, err := os.Stat(m.Dir); !os.IsNotExist(err) {
		t.Errorf("directory survived rm: %v", err)
	}
}

func TestSetOnSuspended(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	for _, args := range [][]string{{"--cpus", "2"}, {"--memory", "4g"}, {"--ssh-port", "2223"}, {"--no-mounts"}} {
		_, err := run(t, root, append([]string{"set", "sl"}, args...)...)
		if err == nil || !strings.Contains(err.Error(), "sl is suspended") ||
			!strings.Contains(formatError("set", "sl", err), "'jm stop sl' restores it and shuts it down") {
			t.Errorf("set %v = %v", args, err)
		}
	}
	_, err := run(t, root, "set", "sl", "--disk", "100")
	if err == nil || !strings.Contains(formatError("set", "sl", err), "run 'jm start sl' first (it grows live)") {
		t.Errorf("set --disk = %v", err)
	}
	if m := mustLoad(t, root, "sl"); m.DiskGiB != 64 || m.CPUs != 4 {
		t.Errorf("refused changes were recorded: %+v", m)
	}
	out, err := run(t, root, "set", "sl", "--arc", "1g", "--idle-suspend", "off")
	if err != nil || !strings.Contains(out, "applied when sl wakes") {
		t.Errorf("set --arc = %q, %v", out, err)
	}
	if m := mustLoad(t, root, "sl"); m.ArcMiB != 1024 || m.IdleSuspendMin != 0 {
		t.Errorf("record = arc %d, idle %d", m.ArcMiB, m.IdleSuspendMin)
	}
	if events := takeFakeEvents(); slices.Contains(events, "resume") {
		t.Errorf("set woke the machine: %v", events)
	}
}

// listenUnix serves a unix socket at a short path, as a stand-in for an
// engine socket that answers.
func listenUnix(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jmt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return path
}

// lastWakeOnly is the wakeOnly argument of the last stubbed wake.
var lastWakeOnly bool

// stubWake replaces startQuietly and returns how many times it ran.
func stubWake(t *testing.T, then func()) *int {
	t.Helper()
	n := new(int)
	old := startQuietlyFn
	startQuietlyFn = func(_ context.Context, _ string, wakeOnly bool) error {
		*n++
		lastWakeOnly = wakeOnly
		if then != nil {
			then()
		}
		return nil
	}
	t.Cleanup(func() { startQuietlyFn = old })
	return n
}

func TestEnsureRunningNotReadyWithJournal(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	m := mustLoad(t, root, "sl")
	oldRoot := stateRoot
	stateRoot = root
	t.Cleanup(func() { stateRoot = oldRoot })
	fakeNet.endpoint = &netprov.Endpoint{SSHHost: "127.0.0.1", SSHPort: 2222, APISocket: listenUnix(t)}
	errOut := captureStderr(t)
	woke := stubWake(t, nil)
	ctx := context.Background()
	var sent []string
	oldReq := sleeperRequest
	sleeperRequest = func(_ context.Context, _ *machine.Machine, line string) (string, error) {
		sent = append(sent, line)
		return "ok", nil
	}
	t.Cleanup(func() { sleeperRequest = oldReq })

	// Running with an answering socket and no journal: ready.
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	if err := ensureRunning(ctx, "sl", true); err != nil || *woke != 0 {
		t.Fatalf("ready machine: %v, wakes %d", err, *woke)
	}
	// The same with a journal: not ready, even with autostart off.
	if err := os.WriteFile(fakeJournal(m), []byte("saving"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRunning(ctx, "sl", false); err != nil || *woke != 1 {
		t.Errorf("running with a journal: %v, wakes %d", err, *woke)
	}
	if !slices.Equal(sent, []string{"abort"}) {
		t.Errorf("a saving journal must send the sleeper an abort: %q", sent)
	}
	// Suspended wakes whatever JM_AUTOSTART says.
	if err := os.WriteFile(fakeJournal(m), []byte("saved"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBE.state, fakeNet.state = backend.Suspended, backend.Stopped
	if err := ensureRunning(ctx, "sl", false); err != nil || *woke != 2 || !lastWakeOnly {
		t.Errorf("suspended with autostart off: %v, wakes %d, wake only %v", err, *woke, lastWakeOnly)
	}
	if len(sent) != 1 {
		t.Errorf("a saved journal must not send an abort: %q", sent)
	}
	if !strings.Contains(errOut.String(), `waking jailmachine "sl"...`) {
		t.Errorf("stderr = %q", errOut.String())
	}
	// Stopped with autostart off is still an error.
	_ = os.Remove(fakeJournal(m))
	fakeBE.state = backend.Stopped
	if err := ensureRunning(ctx, "sl", false); err == nil || !strings.Contains(err.Error(), "autostart is off") || *woke != 2 {
		t.Errorf("stopped with autostart off: %v, wakes %d", err, *woke)
	}
}

func TestSSHWakesSuspended(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	captureStderr(t)
	woke := stubWake(t, func() { fakeBE.state, fakeNet.state = backend.Running, backend.Running })
	var got []string
	old := sshInteractive
	sshInteractive = func(_ string, _ int, _, _ string, args []string) error {
		got = args
		return nil
	}
	t.Cleanup(func() { sshInteractive = old })

	if _, err := run(t, root, "ssh", "sl", "uname", "-a"); err != nil {
		t.Fatal(err)
	}
	if *woke != 1 || !slices.Equal(got, []string{"uname", "-a"}) || !lastWakeOnly {
		t.Errorf("wakes %d, ssh args %q, wake only %v", *woke, got, lastWakeOnly)
	}
	fakeBE.state, fakeNet.state = backend.Stopped, backend.Stopped
	if _, err := run(t, root, "ssh", "sl", "true"); err == nil || !strings.Contains(err.Error(), "not running") || *woke != 1 {
		t.Errorf("ssh on stopped = %v, wakes %d", err, *woke)
	}
}

func TestEnvWarnsSuspended(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	fakeNet.endpoint = &netprov.Endpoint{SSHHost: "127.0.0.1", SSHPort: 2222, APISocket: "/tmp/jm-test.sock"}
	errOut := captureStderr(t)
	out, err := run(t, root, "env", "sl")
	if err != nil || !strings.Contains(out, "DOCKER_HOST=") {
		t.Fatalf("env = %q, %v", out, err)
	}
	if !strings.Contains(errOut.String(), "sl is suspended") || strings.Contains(errOut.String(), "appears once") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if slices.Contains(takeFakeEvents(), "resume") {
		t.Error("env woke the machine")
	}
}

func TestPortsNoteSuspended(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	out, err := run(t, root, "ports", "sl")
	if err != nil || !strings.Contains(out, "these ports answer after the machine wakes; connecting to them does not wake it") {
		t.Errorf("ports = %q, %v", out, err)
	}
	if strings.Contains(out, "is not running") {
		t.Errorf("a suspended machine's forwarder is not a problem to fix:\n%s", out)
	}
}

func TestDoctorSuspendedOKNeverWakes(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	out, _ := run(t, root, "--json", "doctor")
	var rep struct {
		Checks []struct {
			Name, Status, Detail string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	found := false
	for _, c := range rep.Checks {
		switch c.Name {
		case "machine sl":
			found = true
			if c.Status != "ok" || !strings.Contains(c.Detail, "suspended since") || !strings.Contains(c.Detail, "image 1.3 GiB") {
				t.Errorf("machine check = %+v", c)
			}
		case "suspend sl", "clock sl", "guest resolver sl":
			t.Errorf("a suspended machine got a guest check: %+v", c)
		}
	}
	if !found {
		t.Errorf("no machine check:\n%s", out)
	}
	if events := takeFakeEvents(); len(events) != 0 {
		t.Errorf("doctor touched the machine: %v", events)
	}
}

func TestInspectJSONSuspendedKeys(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	m := seedSuspended(t, root, "sl")
	out, err := run(t, root, "--json", "inspect", "sl")
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatal(err)
	}
	if keys["state"] != "suspended" || keys["suspended_at"] != "2026-09-14T14:02:00Z" ||
		keys["suspend_image"] != filepath.Join(m.Dir, machine.SuspendImageFile) || keys["suspend_image_bytes"] != float64(1395864371) {
		t.Errorf("inspect json = %v", keys)
	}
	out, err = run(t, root, "inspect", "sl")
	if err != nil || !regexpState.MatchString(out) {
		t.Errorf("inspect = %q, %v", out, err)
	}

	fakeBE.state = backend.Stopped
	_ = os.Remove(fakeJournal(m))
	out, _ = run(t, root, "--json", "inspect", "sl")
	keys = nil
	_ = json.Unmarshal([]byte(out), &keys)
	for _, k := range []string{"suspended_at", "suspend_image", "suspend_image_bytes"} {
		if _, ok := keys[k]; ok {
			t.Errorf("stopped machine has %s", k)
		}
	}
	for _, k := range []string{"running|stopped|suspended|broken", "suspended_at", "suspend_image ", "suspend_image_bytes"} {
		if !strings.Contains(inspectLong, k) {
			t.Errorf("inspectLong lacks %q", k)
		}
	}
}

var regexpState = regexp.MustCompile(`State:\s+suspended \(since [^;]+; image 1\.3 GiB; wakes on first use\)`)

func TestListHelpMentionsSuspended(t *testing.T) {
	out, err := run(t, t.TempDir(), "list", "--help")
	if err != nil || !strings.Contains(out, "STATE is running, stopped, suspended or broken") ||
		!strings.Contains(out, `.state == "running" or .state == "suspended"`) {
		t.Errorf("list help = %q, %v", out, err)
	}
	out, err = run(t, t.TempDir(), "--help")
	if err != nil || !strings.Contains(out, "\n  suspend ") || !strings.Contains(out, "jm suspend") {
		t.Errorf("root help = %q, %v", out, err)
	}
}

// A forced stop kills the hypervisor even when resolving an interrupted
// transition fails (a woken QEMU whose monitor hangs); a graceful one stops.
func TestStopForceSurvivesRecoveryFailure(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	captureStderr(t)
	fakeBE.state, fakeNet.state = backend.Running, backend.Running
	fakeBE.recoverErr = errors.New("qmp: reading greeting: i/o timeout")

	if _, err := run(t, root, "stop", "sl"); err == nil || !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("graceful stop = %v", err)
	}
	if events := takeFakeEvents(); slices.Contains(events, "stop graceful=true") {
		t.Fatalf("graceful stop went ahead: %v", events)
	}
	if out, err := run(t, root, "stop", "--force", "sl"); err != nil {
		t.Fatalf("stop --force: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if !inOrder(events, "recover", "stop graceful=false", "net-stop") {
		t.Errorf("events = %v", events)
	}
	if fakeBE.state != backend.Stopped {
		t.Errorf("state = %s", fakeBE.state)
	}
}

// A restore for shutdown that fails once the journal is gone leaves a running
// guest and no saved state: it is shut down, not reported as still suspended.
func TestStopOnSuspendedRestoredButNotContinuedPowersOff(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedSuspended(t, root, "sl")
	startFakeGuest(t, root, "sl", defaultGuest)
	errOut := captureStderr(t)
	fakeBE.resumeErr = errors.New("qemu: continuing the restored guest: timeout")
	fakeBE.resumeLeavesRunning = true

	if out, err := run(t, root, "stop", "sl"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	events := takeFakeEvents()
	if !inOrder(events, "resume", "stop graceful=true", "net-stop") || slices.Contains(events, "discard") {
		t.Errorf("events = %v", events)
	}
	if fakeBE.state != backend.Stopped || fakeNet.state != backend.Stopped {
		t.Errorf("states = %s/%s", fakeBE.state, fakeNet.state)
	}
	if strings.Contains(errOut.String(), "discards the saved state") {
		t.Errorf("stderr mentions a saved state that is gone: %q", errOut.String())
	}
}

// A wake that may not boot (jm ssh, a wrapper with autostart off) leaves a
// machine that a concurrent jm stop stopped meanwhile stopped.
func TestWakeOnlyDoesNotBootStopped(t *testing.T) {
	resetFakes(t)
	root := t.TempDir()
	seedFakeRecord(t, root, "sl")
	oldRoot := stateRoot
	stateRoot = root
	t.Cleanup(func() { stateRoot = oldRoot })

	err := runQuietly(context.Background(), "sl", true)
	if err == nil || !strings.Contains(err.Error(), "stopped before it could be woken") {
		t.Fatalf("wake only on a stopped machine = %v", err)
	}
	if events := takeFakeEvents(); slices.Contains(events, "boot") || slices.Contains(events, "net-start") {
		t.Errorf("a wake-only start booted the machine: %v", events)
	}
}

// jm_remount must recognise a mounted share whose path mount -p escapes (a
// space is \040), and must not mount a share whose host path vanished while
// suspended (its tag is gone from the share table).
func TestJmRemountEscapedPathsAndAbsentShares(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeMount := "#!/bin/sh\nif [ \"$1\" = -p ]; then cat \"$JM_TEST_MOUNTED\"; exit 0; fi\necho \"$*\" >> \"$JM_TEST_CALLS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "mount"), []byte(fakeMount), 0o755); err != nil {
		t.Fatal(err)
	}
	mounted := filepath.Join(base, "mounted")
	list := filepath.Join(base, "list")
	tab := filepath.Join(base, "shares.tab")
	calls := filepath.Join(base, "calls")
	conf := machine.GuestConfTag + " " + machine.GuestConfMount + " p9fs ro 0 0\n"
	spaced := "jm0 " + base + "/My\\040Files p9fs rw 0 0\n"
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(mounted, conf+spaced)
	write(list, conf+spaced+"jm1 "+base+"/src p9fs rw 0 0\n"+"jm2 "+base+"/gone p9fs rw 0 0\n")
	write(tab, "# tag\tmountpoint\tmode\njm0\t"+base+"/My Files\trw\njm1\t"+base+"/src\trw\n")

	fn := strings.ReplaceAll(jmRemountFn, machine.GuestSuspendMounts, list)
	fn = strings.ReplaceAll(fn, machine.GuestSharesTab, tab)
	cmd := exec.Command("/bin/sh", "-c", "set -u\n"+fn+"jm_remount\n")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "JM_TEST_MOUNTED=" + mounted, "JM_TEST_CALLS=" + calls}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("jm_remount: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(calls)
	if want := "-t p9fs -o rw jm1 " + base + "/src\n"; string(got) != want {
		t.Errorf("mount calls = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(base, "gone")); !os.IsNotExist(err) {
		t.Errorf("an absent share's mount point was created: %v", err)
	}
}
