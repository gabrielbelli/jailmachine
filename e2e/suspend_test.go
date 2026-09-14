//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/backend/qemu"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov/gvproxy"
	"github.com/gabrielbelli/jailmachine/internal/resolver"
	"github.com/gabrielbelli/jailmachine/internal/sleeper"
)

const (
	suspendMachineName = "e2e-suspend"
	// suspendSSHPort differs from TestLifecycle's 2223, so a machine one
	// test leaves behind never holds the other's port.
	suspendSSHPort = "2224"
	// The sentinel is a detached guest process: it survives a resume and
	// not a cold boot, which /tmp/marker cannot tell apart because the guest
	// keeps /tmp on ZFS and does not clear it at boot. The bracket keeps
	// pgrep's pattern from matching the shell that runs pgrep, and the empty
	// quotes keep the command that starts the sentinel from matching it.
	sentinelCommand = "daemon -f sleep 42424''2"
	sentinelPattern = "'sleep 42424[2]'"
	// The busy-share session's sleep, and its pattern.
	busyCommand = "sleep 3131"
	busyPattern = "'sleep 313[1]'"
	// The idle-timer's command session outlasts the 5-minute idle period.
	idleSessionCommand = "sleep 400"
	idleSessionPattern = "'sleep 40[0]'"
	httpPort           = "8090"

	// lockRetryMax bounds the retries of a suspend refused because another
	// command or transition holds the machine.
	lockRetryMax = 90 * time.Second
	// busyRetryMax bounds the retries of a suspend refused for a command
	// session or engine client that a finished client leaves counted for a
	// moment; busyRetryWarn is how long that may last before it is an error.
	busyRetryMax  = 45 * time.Second
	busyRetryWarn = 15 * time.Second
	// narrowAttempts is how often a kill aimed at a window of milliseconds,
	// or a client aimed at the abortable part of a suspend, is tried.
	narrowAttempts = 3
)

// suspendSuite is one machine shared by TestSuspend's ordered subtests, so
// the guest boots once rather than once per behaviour.
type suspendSuite struct {
	h   *harness
	cfg e2eConfig
	// share is the host directory given to --mount; guestShare is the same
	// directory as the guest sees it (identity path, ADR 0007); cover is the
	// share that exports it, which is a default root when the directory is
	// nested in one ($TMPDIR is shared by default).
	share, guestShare, cover string
	// token is what /tmp/marker and the share files hold.
	token string
}

// TestSuspend drives idle suspend and wake on use (ADR 0009) against one
// real machine: "make build && JM_E2E=1 make e2e". A subtest that fails
// leaves the machine in an unknown state, so the ones after it are skipped;
// ports-after-wake and idle-timer are soft, and a "jm start" converges the
// machine after them instead.
func TestSuspend(t *testing.T) {
	cfg := requireE2E(t)
	h := &harness{bin: cfg.bin, root: t.TempDir(), name: suspendMachineName}
	s := &suspendSuite{h: h, cfg: cfg, share: t.TempDir()}

	// rm --force also takes the guest's containers and both podman
	// connections with it. It has its own limit: the commands before it are
	// capped short of go test's deadline to leave it time.
	t.Cleanup(func() {
		runCmdFor(t, h.command("rm", "--force", h.name), cleanupTimeout)
	})

	h.jmFor(t, provisionTimeout, "init", h.name, "--image", cfg.image, "--disk", strconv.Itoa(cfg.diskGiB), "--memory", "2048",
		"--ssh-port", suspendSSHPort, "--mount", s.share, "--idle-suspend", "0")
	h.jmFor(t, provisionTimeout, "start", h.name)
	s.resolveShare(t)

	steps := []struct {
		name string
		soft bool
		fn   func(*testing.T)
	}{
		// First: a machine woken soon after a suspend has its idle period
		// lengthened (the flap guard), which every later subtest does.
		{"idle-timer", true, s.idleTimer},
		{"suspend-resume", false, s.suspendResume},
		{"wake-on-connect", false, s.wakeOnConnect},
		{"abort-during-suspend", false, s.abortDuringSuspend},
		{"refusals", false, s.refusals},
		{"ports-after-wake", true, s.portsAfterWake},
		{"kill-during-suspend", false, s.killDuringSuspend},
		{"kill-during-wake", false, s.killDuringWake},
		{"reboot-sim", false, s.rebootSim},
		{"incompatible-machine-type", false, s.incompatibleMachineType},
		{"incompatible-disk", false, s.incompatibleDisk},
		{"stop-from-suspended-and-rm", false, s.stopAndRm},
	}
	failed := ""
	for _, step := range steps {
		ok := t.Run(step.name, func(t *testing.T) {
			if failed != "" {
				t.Skipf("skipped: %s failed and left the machine in an unknown state", failed)
			}
			step.fn(t)
		})
		switch {
		case ok || failed != "":
		case step.soft:
			runCmd(t, h.command("start", h.name))
		default:
			failed = step.name
		}
	}
}

// resolveShare finds the guest path of the --mount directory and the share
// that exports it.
func (s *suspendSuite) resolveShare(t *testing.T) {
	t.Helper()
	p, err := filepath.EvalSymlinks(s.share)
	if err != nil {
		t.Fatal(err)
	}
	// jm keeps the /var spelling of /private/var (machine.CanonicalHostPath).
	if strings.HasPrefix(p, "/private/var/") {
		p = strings.TrimPrefix(p, "/private")
	}
	s.guestShare = p
	for _, sh := range s.h.inspect(t).Shares {
		if sh.ReadOnly || (p != sh.GuestPath && !strings.HasPrefix(p, strings.TrimSuffix(sh.GuestPath, "/")+"/")) {
			continue
		}
		if len(sh.GuestPath) > len(s.cover) {
			s.cover = sh.GuestPath
		}
	}
	if s.cover == "" {
		t.Fatalf("no writable share exports %s", p)
	}
	t.Logf("share %s is exported by %s", p, s.cover)
}

// refusalKind sorts a refused suspend by whether trying again can help.
type refusalKind int

const (
	// refusalFinal will not pass on its own: a jail or container, a busy
	// share, the inhibit file, or any other error.
	refusalFinal refusalKind = iota
	// refusalLock is another command or transition holding the machine.
	refusalLock
	// refusalBusy is a command session or engine client and nothing else:
	// what a client that just finished can leave counted for a moment.
	refusalBusy
)

// classifyRefusal sorts the output of a refused jm suspend.
func classifyRefusal(out string) refusalKind {
	switch {
	case strings.Contains(out, "another jm command is operating"),
		strings.Contains(out, "is already in progress"),
		strings.Contains(out, "cancelled: a client arrived"):
		return refusalLock
	case !strings.Contains(out, " is busy: "),
		strings.Contains(out, "or container"), // "1 jail or container running"
		strings.Contains(out, "shared directory"),
		strings.Contains(out, "inhibit"),
		strings.Contains(out, " exists"): // the inhibit file
		return refusalFinal
	case strings.Contains(out, "command session"),
		strings.Contains(out, "engine client"),
		strings.Contains(out, "jails=0 "): // the quiesce's own count
		return refusalBusy
	}
	return refusalFinal
}

// retrier decides whether a refused suspend is tried again.
type retrier struct {
	began     time.Time
	busySince time.Time
}

func newRetrier() *retrier { return &retrier{began: time.Now()} }

// again reports whether to try a suspend refused with output out again,
// after sleeping a little.
func (r *retrier) again(out string) bool {
	switch classifyRefusal(out) {
	case refusalLock:
		if time.Since(r.began) < lockRetryMax {
			time.Sleep(3 * time.Second)
			return true
		}
	case refusalBusy:
		if r.busySince.IsZero() {
			r.busySince = time.Now()
		}
		if time.Since(r.busySince) < busyRetryMax {
			time.Sleep(2 * time.Second)
			return true
		}
	}
	return false
}

// check fails t when the guest took longer than busyRetryWarn to stop
// counting a finished client as activity: automatic suspend would then be
// held off after every use.
func (r *retrier) check(t *testing.T) {
	t.Helper()
	if r.busySince.IsZero() {
		return
	}
	if d := time.Since(r.busySince); d > busyRetryWarn {
		t.Errorf("the guest counted a finished client as activity for %s, want at most %s", d.Round(time.Second), busyRetryWarn)
	}
}

// suspend runs "jm suspend" until it succeeds, retrying only the refusals
// that pass on their own, and fails t on anything else. The machine must
// then read suspended.
func (s *suspendSuite) suspend(t *testing.T) {
	t.Helper()
	h := s.h
	rt := newRetrier()
	for {
		r := h.run(t, "suspend", h.name)
		if r.err == nil {
			break
		}
		if !rt.again(r.combined()) {
			t.Fatalf("jm suspend: %v", r.err)
		}
	}
	rt.check(t)
	h.wantState(t, "suspended")
}

// ownedPID is the live process a pid file in the machine directory names,
// when that process belongs to this machine: its command line names the
// state root or one of the machine's sockets. A recycled pid never counts,
// and is never signalled.
func (s *suspendSuite) ownedPID(path string) (int, bool) {
	pid, ok := pidFileAlive(path)
	if !ok {
		return 0, false
	}
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	cmdline := string(out)
	dir := s.h.dir()
	markers := []string{s.h.root, qemu.QMPSocket(dir)}
	markers = append(markers, gvproxy.PathsFor(dir).Sockets()...)
	for _, m := range markers {
		if strings.Contains(cmdline, m) {
			return pid, true
		}
	}
	return 0, false
}

// journalPath is the suspend journal's path.
func (s *suspendSuite) journalPath() string {
	return filepath.Join(s.h.dir(), machine.SuspendJournalFile)
}

// readJournal decodes the suspend journal, failing t when it cannot.
func (s *suspendSuite) readJournal(t *testing.T) qemu.Journal {
	t.Helper()
	data, err := os.ReadFile(s.journalPath())
	if err != nil {
		t.Fatalf("reading %s: %v", machine.SuspendJournalFile, err)
	}
	var j qemu.Journal
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatalf("decoding %s: %v", machine.SuspendJournalFile, err)
	}
	return j
}

// editJournal rewrites the suspend journal through edit. Every field edit
// leaves alone stays raw JSON: decoding it into interface values would turn
// the fingerprints' int64 nanosecond mtimes into float64 and round them, and
// jm would then discard the state for a changed disk.raw instead of the
// reason under test. The fingerprints are checked to survive the edit.
func (s *suspendSuite) editJournal(t *testing.T, edit func(j map[string]json.RawMessage)) {
	t.Helper()
	before := s.readJournal(t)
	data, err := os.ReadFile(s.journalPath())
	if err != nil {
		t.Fatal(err)
	}
	var j map[string]json.RawMessage
	if err := json.Unmarshal(data, &j); err != nil {
		t.Fatalf("decoding %s: %v", machine.SuspendJournalFile, err)
	}
	edit(j)
	out, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.journalPath(), append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	after := s.readJournal(t)
	if (before.Fingerprints == nil) != (after.Fingerprints == nil) ||
		(before.Fingerprints != nil && *before.Fingerprints != *after.Fingerprints) {
		t.Fatalf("editing %s changed its fingerprints: %+v, then %+v", machine.SuspendJournalFile, before.Fingerprints, after.Fingerprints)
	}
}

// journalPhase reads the journal's phase without failing: "" while it is
// absent or being replaced.
func (s *suspendSuite) journalPhase() string {
	data, err := os.ReadFile(s.journalPath())
	if err != nil {
		return ""
	}
	var j struct {
		Phase string `json:"phase"`
	}
	if json.Unmarshal(data, &j) != nil {
		return ""
	}
	return j.Phase
}

// wantSuspendedFiles checks the host side of a suspended machine: no
// hypervisor, provider, socket forward, forwarder or resolver; a live sleeper
// holding the engine socket; a saved journal and its image.
func (s *suspendSuite) wantSuspendedFiles(t *testing.T) {
	t.Helper()
	h := s.h
	dir := h.dir()
	for _, f := range []string{qemu.PIDFile, gvproxy.PIDFile, gvproxy.ForwardPIDFile, forwarder.PIDFile, resolver.PIDFile} {
		if pid, ok := s.ownedPID(filepath.Join(dir, f)); ok {
			t.Errorf("%s names live pid %d while suspended", f, pid)
		}
	}
	if _, ok := s.ownedPID(filepath.Join(dir, sleeper.PIDFile)); !ok {
		t.Errorf("no live sleeper (%s) while suspended", sleeper.PIDFile)
	}
	if phase := s.journalPhase(); phase != string(backend.SuspendSaved) {
		t.Errorf("%s phase %q, want %q", machine.SuspendJournalFile, phase, backend.SuspendSaved)
	}
	if st, err := os.Stat(filepath.Join(dir, machine.SuspendImageFile)); err != nil || st.Size() == 0 {
		t.Errorf("%s missing or empty while suspended: %v", machine.SuspendImageFile, err)
	}
	i := h.inspect(t)
	if i.SleeperState != "running" {
		t.Errorf("inspect sleeper_state %q, want running", i.SleeperState)
	}
	if i.SuspendImage == "" {
		t.Errorf("inspect reports no suspend_image while suspended")
	}
	if i.APISocket == "" {
		t.Errorf("inspect reports no api_socket")
	} else if st, err := os.Stat(i.APISocket); err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Errorf("%s is not a socket while suspended (the sleeper holds it): %v", i.APISocket, err)
	}
}

// wantNoSavedState checks that nothing of a suspend is left: no journal, no
// image, no partial image, no journal set aside as unreadable.
func (s *suspendSuite) wantNoSavedState(t *testing.T) {
	t.Helper()
	for _, f := range []string{machine.SuspendJournalFile, machine.SuspendImageFile, qemu.SuspendTmpFile, qemu.BadJournalFile} {
		if _, err := os.Stat(filepath.Join(s.h.dir(), f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still present: %v", f, err)
		}
	}
}

// startSentinel starts the boot-scoped guest process if it is not running.
func (s *suspendSuite) startSentinel(t *testing.T) {
	t.Helper()
	s.h.mustGuest(t, "pgrep -f "+sentinelPattern+" >/dev/null || "+sentinelCommand)
	s.wantSentinel(t, true)
}

// wantSentinel fails t unless the sentinel's liveness is want: alive after a
// resume, gone after a cold boot.
func (s *suspendSuite) wantSentinel(t *testing.T, want bool) {
	t.Helper()
	r := s.h.guest(t, "pgrep -f "+sentinelPattern)
	code := r.exitCode()
	if code != 0 && code != 1 {
		t.Fatalf("pgrep in the guest: %v", r.err)
	}
	if alive := code == 0; alive != want {
		t.Fatalf("guest sentinel process alive=%v, want %v (alive means the guest was resumed, not booted)", alive, want)
	}
}

// wantMarker fails t unless /tmp/marker holds the token.
func (s *suspendSuite) wantMarker(t *testing.T) {
	t.Helper()
	if got := s.h.mustGuest(t, "cat /tmp/marker"); got != s.token {
		t.Fatalf("/tmp/marker holds %q, want %q", got, s.token)
	}
}

// wantShareParity checks that the share is mounted in the guest and that a
// file crosses it in each direction.
func (s *suspendSuite) wantShareParity(t *testing.T) {
	t.Helper()
	h := s.h
	out := h.mustGuest(t, "mount -p -t p9fs")
	mounted := false
	for _, l := range strings.Split(out, "\n") {
		// fstab format, tab-separated: tag, mount point, type, options.
		if f := strings.Fields(l); len(f) >= 2 && f[1] == s.cover {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("%s is not mounted in the guest:\n%s", s.cover, out)
	}
	if err := os.WriteFile(filepath.Join(s.share, "from-host.txt"), []byte(s.token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.mustGuest(t, "cat "+shellQuote(filepath.Join(s.guestShare, "from-host.txt"))); got != s.token {
		t.Fatalf("guest reads %q from the share, want %q", got, s.token)
	}
	word := "guest-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	h.mustGuest(t, "echo "+word+" > "+shellQuote(filepath.Join(s.guestShare, "from-guest.txt")))
	data, err := os.ReadFile(filepath.Join(s.share, "from-guest.txt"))
	if err != nil || strings.TrimSpace(string(data)) != word {
		t.Fatalf("host reads %q from the share, want %q: %v", data, word, err)
	}
}

// wantEngineSocket checks the host engine socket end to end: the libpod
// ping answers on api_socket, and podman lists containers through the
// socket connection. jm ssh never touches this path, so a start that leaves
// a dead stand-in socket in place is only caught here.
func (s *suspendSuite) wantEngineSocket(t *testing.T) {
	t.Helper()
	h := s.h
	sock := h.inspect(t).APISocket
	if sock == "" {
		t.Fatal("inspect reports no api_socket")
	}
	r := runCmd(t, exec.Command("curl", "-sS", "-f", "--max-time", "60", "--unix-socket", sock, "http://d/v5.0.0/libpod/_ping"))
	if r.err != nil || strings.TrimSpace(r.stdout) != "OK" {
		t.Fatalf("libpod _ping over %s: %q, %v", sock, r.stdout, r.err)
	}
	podman(t, h.name+"-sock", "ps")
}

// wantClock checks that the guest clock is within 2 s of the host's.
func (s *suspendSuite) wantClock(t *testing.T) {
	t.Helper()
	before := time.Now().Unix()
	out := s.h.mustGuest(t, "date -u +%s")
	after := time.Now().Unix()
	g, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		t.Fatalf("guest date: %q", out)
	}
	if g < before-2 || g > after+2 {
		t.Errorf("guest clock %d is more than 2 s from the host's (%d to %d)", g, before, after)
	}
}

// wantRecovered checks a machine brought back after an interrupted suspend
// or wake: running with nothing of the suspend left, the guest resumed with
// its processes and files, the shares and engine socket working, the pool
// healthy.
func (s *suspendSuite) wantRecovered(t *testing.T) {
	t.Helper()
	h := s.h
	h.wantState(t, "running")
	s.wantNoSavedState(t)
	s.wantMarker(t)
	s.wantSentinel(t, true)
	s.wantShareParity(t)
	s.wantEngineSocket(t)
	if out := h.mustGuest(t, "zpool status -x"); !strings.Contains(out, "all pools are healthy") {
		t.Errorf("zpool status -x: %s", out)
	}
}

// idleTimer: with --idle-suspend 5, a command session longer than the idle
// period keeps the machine awake; once it ends, the idle monitor suspends
// the machine within 8 minutes.
func (s *suspendSuite) idleTimer(t *testing.T) {
	if !s.cfg.slow {
		t.Skip("set JM_E2E_SLOW=1 to wait for the idle timer (about 15 min)")
	}
	h := s.h
	h.jm(t, "set", h.name, "--idle-suspend", "5")
	defer runCmd(t, h.command("set", h.name, "--idle-suspend", "0"))
	state := func() string {
		out, err := output(h.command("--json", "inspect", h.name), time.Minute)
		var i machineInfo
		if err != nil || json.Unmarshal([]byte(out), &i) != nil {
			return ""
		}
		return i.State
	}

	// Never while a command session is open.
	session := startBackground(t, h.command("ssh", h.name, "--", idleSessionCommand), 15*time.Minute)
	if !waitFor(60*time.Second, time.Second, func() bool {
		_, err := output(h.command("ssh", h.name, "--", "pgrep -f "+idleSessionPattern), 30*time.Second)
		return err == nil
	}) {
		t.Fatalf("the command session never started in the guest: %s", session.out.String())
	}
	opened := time.Now()
	for !session.exited() {
		if state() == "suspended" {
			t.Fatalf("the idle monitor suspended %s %s into an open command session", h.name, time.Since(opened).Round(time.Second))
		}
		select {
		case <-session.done:
		case <-time.After(10 * time.Second):
		}
	}
	if st := state(); st == "suspended" {
		t.Fatalf("the idle monitor suspended %s during a command session (the session ended: %v)", h.name, session.err)
	}
	if session.err != nil || time.Since(opened) < 6*time.Minute {
		t.Fatalf("the command session ended after %s with %v, before it proved anything: %s",
			time.Since(opened).Round(time.Second), session.err, session.out.String())
	}
	t.Logf("no suspend during a %s command session", time.Since(opened).Round(time.Second))

	// Then, with nothing running, it does.
	began := time.Now()
	if !waitFor(8*time.Minute, 10*time.Second, func() bool { return state() == "suspended" }) {
		r := h.run(t, "inspect", h.name)
		t.Fatalf("not suspended %s after the session ended, with --idle-suspend 5\n%s", time.Since(began).Round(time.Second), r.combined())
	}
	t.Logf("the idle monitor suspended %s %s after the session ended", h.name, time.Since(began).Round(time.Second))
	h.wantState(t, "suspended")
	h.jm(t, "set", h.name, "--idle-suspend", "0")
	h.jm(t, "start", h.name)
	h.wantState(t, "running")
}

// suspendResume: a manual suspend ends every host process but the sleeper
// and leaves a saved state; jm ssh wakes the machine with its guest state,
// shares and clock as they should be.
func (s *suspendSuite) suspendResume(t *testing.T) {
	h := s.h
	s.token = "marker-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	h.mustGuest(t, "echo "+s.token+" > /tmp/marker")
	s.startSentinel(t)
	if err := os.WriteFile(filepath.Join(s.share, "before-suspend.txt"), []byte(s.token+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.suspend(t)
	s.wantSuspendedFiles(t)

	r := h.guest(t, "cat /tmp/marker")
	if r.err != nil {
		t.Fatalf("jm ssh on a suspended machine: %v", r.err)
	}
	t.Logf("jm ssh woke %s and ran in %.1f s", h.name, r.took.Seconds())
	if got := strings.TrimSpace(r.stdout); got != s.token {
		t.Fatalf("/tmp/marker after the wake holds %q, want %q", got, s.token)
	}
	if r.took > 15*time.Second {
		t.Errorf("jm ssh took %.1f s to wake the machine, want at most 15 s", r.took.Seconds())
	}
	h.wantState(t, "running")
	s.wantNoSavedState(t)
	s.wantSentinel(t, true)
	if got := h.mustGuest(t, "cat "+shellQuote(filepath.Join(s.guestShare, "before-suspend.txt"))); got != s.token {
		t.Fatalf("guest reads %q from a file written before the suspend, want %q", got, s.token)
	}
	s.wantShareParity(t)
	s.wantClock(t)
	// The clock is stepped at the wake; it must also stay right.
	time.Sleep(30 * time.Second)
	s.wantClock(t)
}

// wakeOnConnect: each client of the machine's endpoints wakes a suspended
// machine and is served.
func (s *suspendSuite) wakeOnConnect(t *testing.T) {
	h := s.h
	sock := h.inspect(t).APISocket
	if sock == "" {
		t.Fatal("inspect reports no api_socket")
	}
	type client struct {
		name string
		cmd  func() *exec.Cmd
		want string // expected trimmed stdout, when not empty
	}
	clients := []client{
		{"curl _ping over the engine socket", func() *exec.Cmd {
			return exec.Command("curl", "-sS", "-f", "--max-time", "120", "--unix-socket", sock, "http://d/v5.0.0/libpod/_ping")
		}, "OK"},
		{"podman --connection " + h.name + "-sock ps", func() *exec.Cmd {
			return exec.Command("podman", "--connection", h.name+"-sock", "ps")
		}, ""},
		{"podman --connection " + h.name + " ps (ssh://)", func() *exec.Cmd {
			return exec.Command("podman", "--connection", h.name, "ps")
		}, ""},
		{"jpodman ps", func() *exec.Cmd { return h.wrapper("podman", "ps") }, ""},
	}
	if _, err := exec.LookPath("docker"); err == nil {
		dockerEnv := append(os.Environ(), "DOCKER_HOST=unix://"+sock)
		docker := func(args ...string) func() *exec.Cmd {
			return func() *exec.Cmd {
				c := exec.Command("docker", args...)
				c.Env = dockerEnv
				return c
			}
		}
		clients = append(clients,
			client{"DOCKER_HOST=unix://… docker version", docker("version"), ""},
			client{"DOCKER_HOST=unix://… docker ps", docker("ps"), ""})
		if _, err := output(exec.Command("docker", "compose", "version"), 30*time.Second); err == nil {
			clients = append(clients, client{"DOCKER_HOST=unix://… docker compose ls", docker("compose", "ls"), ""})
		} else {
			t.Log("docker compose is not installed; skipping docker compose ls")
		}
	} else {
		t.Log("docker is not on PATH; skipping the docker clients")
	}
	for _, c := range clients {
		s.suspend(t)
		r := runCmd(t, c.cmd())
		if r.err != nil {
			t.Fatalf("%s against a suspended machine: %v", c.name, r.err)
		}
		if c.want != "" && strings.TrimSpace(r.stdout) != c.want {
			t.Fatalf("%s printed %q, want %q", c.name, r.stdout, c.want)
		}
		t.Logf("%s woke %s and was served in %.1f s", c.name, h.name, r.took.Seconds())
		h.wantState(t, "running")
	}
	s.wantMarker(t)
}

// abortDuringSuspend: an engine client that arrives once the stand-in holds
// the socket, and before the guest is frozen, cancels the suspend and is
// served by the guest that keeps running.
func (s *suspendSuite) abortDuringSuspend(t *testing.T) {
	h := s.h
	sock := h.inspect(t).APISocket
	if sock == "" {
		t.Fatal("inspect reports no api_socket")
	}
	rt := newRetrier()
	for attempt := 1; attempt <= narrowAttempts; {
		before, ok := inodeOf(sock)
		if !ok {
			t.Fatalf("%s does not exist on a running machine", sock)
		}
		sus := startBackground(t, h.command("suspend", h.name), limit(t, cmdTimeout))
		// The stand-in renames its own socket over the path (S6).
		taken := false
		for !taken && !sus.exited() {
			if ino, ok := inodeOf(sock); ok && ino != before {
				taken = true
			} else {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if !taken {
			<-sus.done
			out := sus.out.String()
			if sus.err != nil && rt.again(out) {
				continue
			}
			t.Fatalf("jm suspend ended (%v) without the stand-in taking %s:\n%s", sus.err, sock, out)
		}
		ping := runCmd(t, exec.Command("curl", "-sS", "-f", "--max-time", "120", "--unix-socket", sock, "http://d/v5.0.0/libpod/_ping"))
		<-sus.done
		out := sus.out.String()
		t.Logf("attempt %d: jm suspend: %v\n%s", attempt, sus.err, out)
		if ping.err != nil || strings.TrimSpace(ping.stdout) != "OK" {
			t.Fatalf("the client that arrived during the suspend was not served: %q, %v", ping.stdout, ping.err)
		}
		if sus.err == nil {
			// The client came after the freeze and woke the machine instead.
			t.Logf("attempt %d: the client arrived after the freeze; trying again", attempt)
			h.wantState(t, "running")
			attempt++
			continue
		}
		if want := "suspend of " + h.name + " cancelled: a client arrived"; !strings.Contains(out, want) {
			t.Fatalf("jm suspend failed without %q", want)
		}
		h.wantState(t, "running")
		s.wantNoSavedState(t)
		s.wantSentinel(t, true)
		s.wantMarker(t)
		s.wantShareParity(t)
		s.wantEngineSocket(t)
		return
	}
	t.Errorf("in %d attempts no client reached the stand-in before the guest was frozen", narrowAttempts)
}

// refusals: a running container, and a guest process inside a share even
// with --force, refuse the suspend and leave the machine running.
func (s *suspendSuite) refusals(t *testing.T) {
	h := s.h

	podman(t, h.name, "run", "-d", "--name", "e2e-sleep", "--os=linux", "docker.io/busybox", "sleep", "3600")
	r := h.run(t, "suspend", h.name)
	if r.err == nil {
		t.Fatal("jm suspend succeeded with a container running")
	}
	if code := r.exitCode(); code != 1 {
		t.Errorf("jm suspend exited %d with a container running, want 1", code)
	}
	if want := h.name + " is busy: 1 jail or container running"; !strings.Contains(r.combined(), want) {
		t.Errorf("jm suspend output lacks %q", want)
	}
	h.wantState(t, "running")
	if fileExists(s.journalPath()) {
		t.Errorf("a refused suspend left %s behind", machine.SuspendJournalFile)
	}
	podman(t, h.name, "rm", "-f", "-t", "0", "e2e-sleep")

	// A command session whose working directory is inside the share.
	bg := startBackground(t, h.command("ssh", h.name, "--", "cd "+shellQuote(s.guestShare)+" && "+busyCommand), limit(t, cmdTimeout))
	endSession := func() {
		_, _ = output(h.command("ssh", h.name, "--", "pkill -f "+busyPattern), time.Minute)
		select {
		case <-bg.done:
		case <-time.After(30 * time.Second):
			bg.kill()
		}
	}
	defer endSession()
	if !waitFor(60*time.Second, time.Second, func() bool {
		_, err := output(h.command("ssh", h.name, "--", "pgrep -f "+busyPattern), 30*time.Second)
		return err == nil
	}) {
		t.Fatalf("the background session never started in the guest: %s", bg.out.String())
	}

	r = h.run(t, "suspend", "--force", h.name)
	if r.err == nil {
		t.Fatal("jm suspend --force succeeded with a guest process inside a share")
	}
	if code := r.exitCode(); code != 1 {
		t.Errorf("jm suspend --force exited %d with a share in use, want 1", code)
	}
	if want := h.name + " is busy: a shared directory is in use in the guest: " + s.cover; !strings.Contains(r.combined(), want) {
		t.Errorf("jm suspend --force output lacks %q", want)
	}
	h.wantState(t, "running")
	if fileExists(s.journalPath()) {
		t.Errorf("a refused suspend left %s behind", machine.SuspendJournalFile)
	}
	endSession()
	t.Logf("background session output: %s", bg.out.String())
	// The rollback mounted every share it had unmounted, and gave the engine
	// socket back from the stand-in to the forward.
	s.wantShareParity(t)
	s.wantMarker(t)
	s.wantEngineSocket(t)
}

// portsAfterWake: a -p container run against a suspended machine wakes it,
// and its port is published within 60 s.
func (s *suspendSuite) portsAfterWake(t *testing.T) {
	h := s.h
	s.suspend(t)
	defer runCmd(t, exec.Command("podman", "--connection", h.name, "rm", "-f", "-t", "0", "e2e-web"))
	began := time.Now()
	podman(t, h.name, append([]string{"run", "-d", "--name", "e2e-web", "-p", httpPort + ":80"}, httpdArgs...)...)
	t.Logf("podman run woke %s and started the container in %.1f s", h.name, time.Since(began).Seconds())
	url := "http://127.0.0.1:" + httpPort + "/"
	if !waitFor(60*time.Second, 2*time.Second, func() bool { return curlOK(url) }) {
		t.Fatalf("%s did not answer within 60 s of the wake\nports: %s", url, h.run(t, "ports", h.name).combined())
	}
	podman(t, h.name, "rm", "-f", "-t", "0", "e2e-web")
	if !waitFor(30*time.Second, 2*time.Second, func() bool { return !curlOK(url) }) {
		t.Errorf("%s still answers 30 s after the container was removed", url)
	}
}

// killDuringSuspend: the sleeper running a suspend is SIGKILLed at four
// points, and jm start brings the guest back each time with nothing lost:
// rolled back before the commit, resumed from the image after it.
func (s *suspendSuite) killDuringSuspend(t *testing.T) {
	h := s.h
	dir := h.dir()
	saving := func() bool { return s.journalPhase() == string(backend.SuspendSaving) }
	kills := []struct {
		name string
		// narrow windows last milliseconds: a miss is retried, and after
		// narrowAttempts misses only logged.
		narrow  bool
		reached func(qemuPID int) bool
	}{
		{"journal written (saving)", false, func(int) bool { return saving() }},
		{"migration writing the image", false, func(int) bool {
			return saving() && fileExists(filepath.Join(dir, qemu.SuspendTmpFile))
		}},
		{"image renamed, journal not committed", true, func(int) bool {
			return saving() && fileExists(filepath.Join(dir, machine.SuspendImageFile))
		}},
		{"journal committed, QEMU still running", true, func(qemuPID int) bool {
			return s.journalPhase() == string(backend.SuspendSaved) && qemuPID > 0 && syscall.Kill(qemuPID, 0) == nil
		}},
	}
	for _, k := range kills {
		t.Logf("--- SIGKILL the sleeper at: %s", k.name)
		landed := false
		for attempt := 1; attempt <= narrowAttempts && !landed; attempt++ {
			landed = s.killSuspendWhen(t, k.name, k.reached)
			if !landed && !k.narrow {
				t.Fatalf("jm suspend completed before: %s", k.name)
			}
			if !landed {
				t.Logf("attempt %d: jm suspend completed before %s; waking and trying again", attempt, k.name)
			}
			t.Logf("state before jm start: %q", h.listState(t))
			h.jm(t, "start", h.name)
			s.wantRecovered(t)
		}
		if !landed {
			t.Logf("the window %q was missed %d times; that case was not exercised in this run", k.name, narrowAttempts)
		}
	}
}

// killSuspendWhen starts "jm suspend" and, as soon as reached is true,
// SIGKILLs the sleeper running the suspend and then jm suspend. It reports
// false when jm suspend succeeded first; refusals that pass on their own
// are retried.
func (s *suspendSuite) killSuspendWhen(t *testing.T, name string, reached func(qemuPID int) bool) bool {
	t.Helper()
	h := s.h
	dir := h.dir()
	sleeperFile := filepath.Join(dir, sleeper.PIDFile)
	rt := newRetrier()
	for {
		// Resolved up front: at the kill there is no time for ps.
		qemuPID, _ := s.ownedPID(filepath.Join(dir, qemu.PIDFile))
		sleeperPID, sleeperOK := s.ownedPID(sleeperFile)
		sus := startBackground(t, h.command("suspend", h.name), limit(t, cmdTimeout))
		hit := false
		for !hit && !sus.exited() {
			if hit = reached(qemuPID); !hit {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if hit {
			// jm suspend may have started the sleeper itself.
			if pid, ok := pidFromFile(sleeperFile); !sleeperOK || !ok || pid != sleeperPID {
				sleeperPID, sleeperOK = s.ownedPID(sleeperFile)
			}
			if !sleeperOK {
				sus.kill()
				t.Fatalf("no live sleeper at: %s", name)
			}
			err := syscall.Kill(sleeperPID, syscall.SIGKILL)
			qemuLive := qemuPID > 0 && syscall.Kill(qemuPID, 0) == nil
			sus.kill()
			if err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("SIGKILL sleeper %d: %v", sleeperPID, err)
			}
			t.Logf("SIGKILLed the sleeper (pid %d) and jm suspend at: %s; QEMU (pid %d) alive just after: %v\njm suspend output:\n%s",
				sleeperPID, name, qemuPID, qemuLive, sus.out.String())
			return true
		}
		out := sus.out.String()
		if sus.err == nil {
			t.Logf("jm suspend completed before: %s\n%s", name, out)
			return false
		}
		if rt.again(out) {
			continue
		}
		t.Fatalf("jm suspend failed before %s: %v\n%s", name, sus.err, out)
	}
}

// killDuringWake: the jm start waking a suspended machine is SIGKILLed once
// the network provider runs, once QEMU runs, and once the loaded state's
// journal is gone; the next jm start finishes the wake with nothing lost.
func (s *suspendSuite) killDuringWake(t *testing.T) {
	h := s.h
	dir := h.dir()
	kills := []struct {
		name    string
		reached func() bool
	}{
		{"gvproxy started", func() bool { _, ok := s.ownedPID(filepath.Join(dir, gvproxy.PIDFile)); return ok }},
		{"QEMU started", func() bool { _, ok := s.ownedPID(filepath.Join(dir, qemu.PIDFile)); return ok }},
		{"journal removed", func() bool { return !fileExists(s.journalPath()) }},
	}
	for _, k := range kills {
		t.Logf("--- SIGKILL jm start at: %s", k.name)
		landed := false
		for attempt := 1; attempt <= narrowAttempts && !landed; attempt++ {
			s.suspend(t)
			wake := startBackground(t, h.command("start", h.name), limit(t, cmdTimeout))
			hit := false
			for !hit && !wake.exited() {
				if hit = k.reached(); !hit {
					time.Sleep(2 * time.Millisecond)
				}
			}
			wake.kill()
			// A wake that completed before the kill exits 0.
			landed = hit && wake.err != nil
			t.Logf("attempt %d: kill at %s landed: %v (jm start: %v)\n%s", attempt, k.name, landed, wake.err, wake.out.String())
			t.Logf("state before jm start: %q", h.listState(t))
			h.jm(t, "start", h.name)
			s.wantRecovered(t)
		}
		if !landed {
			t.Errorf("jm start finished before %s in %d attempts; that case was not exercised", k.name, narrowAttempts)
		}
	}
}

// rebootSim: with every host process of a suspended machine killed and its
// sockets gone, as after a Mac restart, the machine still reads suspended and
// the first jpodman command resumes it.
func (s *suspendSuite) rebootSim(t *testing.T) {
	h := s.h
	s.suspend(t)
	dir := h.dir()
	pidFiles, err := filepath.Glob(filepath.Join(dir, "*.pid"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range pidFiles {
		pid, ok := s.ownedPID(f)
		if !ok {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatalf("SIGKILL %s pid %d: %v", filepath.Base(f), pid, err)
		}
		if !waitFor(10*time.Second, 100*time.Millisecond, func() bool { return !processAlive(pid) }) {
			t.Fatalf("%s pid %d survived SIGKILL", filepath.Base(f), pid)
		}
		t.Logf("SIGKILLed %s pid %d", filepath.Base(f), pid)
	}
	for _, sock := range []string{qemu.QMPSocket(dir), gvproxy.PathsFor(dir).Podman} {
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if got := h.listState(t); got != "suspended" {
		t.Fatalf("jm --json list after the simulated reboot: %q, want suspended", got)
	}
	r := runCmd(t, h.wrapper("podman", "ps"))
	if r.err != nil {
		t.Fatalf("jpodman ps after the simulated reboot: %v", r.err)
	}
	t.Logf("jpodman ps resumed %s in %.1f s", h.name, r.took.Seconds())
	s.wantRecovered(t)
}

// incompatibleMachineType: a saved state for a machine type QEMU does not
// have is discarded with a warning that names the cause, and the machine
// boots from disk in the same command.
func (s *suspendSuite) incompatibleMachineType(t *testing.T) {
	h := s.h
	s.suspend(t)
	s.editJournal(t, func(j map[string]json.RawMessage) {
		j["machine_type"] = json.RawMessage(`"virt-99.0"`)
		var argv []string
		if err := json.Unmarshal(j["argv"], &argv); err != nil {
			t.Fatalf("decoding the saved argv: %v", err)
		}
		edited := false
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "-M" || argv[i] == "-machine" {
				nv := "virt-99.0"
				if _, rest, ok := strings.Cut(argv[i+1], ","); ok {
					nv += "," + rest
				}
				argv[i+1] = nv
				edited = true
			}
		}
		if !edited {
			t.Fatalf("no -M in the saved argv: %q", argv)
		}
		data, err := json.Marshal(argv)
		if err != nil {
			t.Fatal(err)
		}
		j["argv"] = data
	})

	r := h.run(t, "start", h.name)
	if r.err != nil {
		t.Fatalf("jm start with an incompatible saved state: %v", r.err)
	}
	out := r.combined()
	for _, want := range []string{"could not restore the suspended state of " + h.name, "virt-99.0", "booting from disk"} {
		if !strings.Contains(out, want) {
			t.Errorf("jm start output lacks %q", want)
		}
	}
	if !strings.Contains(strings.ToLower(out), "unsupported machine type") {
		t.Errorf("jm start output does not give QEMU's unsupported machine type as the cause")
	}
	if strings.Contains(out, "changed after the state was saved") {
		t.Errorf("jm start discarded the state for a changed file, not for the machine type")
	}
	h.wantState(t, "running")
	s.wantNoSavedState(t)
	s.wantSentinel(t, false)
	// Not asserted: the guest keeps /tmp on ZFS and does not clear it at
	// boot, so /tmp/marker outlives a cold boot. The sentinel above is the
	// boot marker.
	m := h.guest(t, "cat /tmp/marker")
	t.Logf("/tmp/marker after the cold boot: exit %d, %q", m.exitCode(), strings.TrimSpace(m.stdout))
	s.wantShareParity(t)
	s.wantEngineSocket(t)
}

// incompatibleDisk: a disk.raw modified while suspended invalidates the
// saved state, which is discarded with a warning naming the disk; the
// machine boots from disk in the same command.
func (s *suspendSuite) incompatibleDisk(t *testing.T) {
	h := s.h
	s.startSentinel(t)
	s.suspend(t)
	if j := s.readJournal(t); j.Fingerprints == nil {
		t.Fatalf("the journal of a completed suspend has no fingerprints, so a changed %s would go unnoticed", machine.DiskFile)
	}
	disk := filepath.Join(h.dir(), machine.DiskFile)
	st, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	later := st.ModTime().Add(time.Second)
	if err := os.Chtimes(disk, later, later); err != nil {
		t.Fatal(err)
	}

	r := h.run(t, "start", h.name)
	if r.err != nil {
		t.Fatalf("jm start after %s changed: %v", machine.DiskFile, r.err)
	}
	for _, want := range []string{"could not restore the suspended state of " + h.name,
		machine.DiskFile + " changed after the state was saved", "booting from disk"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("jm start output lacks %q", want)
		}
	}
	h.wantState(t, "running")
	s.wantNoSavedState(t)
	s.wantSentinel(t, false)
	s.wantShareParity(t)
	s.wantEngineSocket(t)
}

// stopAndRm: jm stop on a suspended machine ends stopped with no process
// left, and jm rm removes the machine and its podman connections.
func (s *suspendSuite) stopAndRm(t *testing.T) {
	h := s.h
	s.suspend(t)
	h.jm(t, "stop", h.name)
	h.wantState(t, "stopped")
	pidFiles, err := filepath.Glob(filepath.Join(h.dir(), "*.pid"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range pidFiles {
		if pid, ok := s.ownedPID(f); ok {
			t.Errorf("%s names live pid %d after jm stop", filepath.Base(f), pid)
		}
	}
	s.wantNoSavedState(t)

	h.jm(t, "rm", h.name)
	if _, err := os.Stat(h.dir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("machine directory still present after rm: %v", err)
	}
	r := runCmd(t, exec.Command("podman", "system", "connection", "list", "--format", "{{.Name}}"))
	if r.err != nil {
		t.Fatalf("podman system connection list: %v", r.err)
	}
	for _, l := range strings.Split(r.stdout, "\n") {
		if l = strings.TrimSpace(l); l == h.name || l == h.name+"-sock" {
			t.Errorf("podman connection %s still present after rm", l)
		}
	}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
