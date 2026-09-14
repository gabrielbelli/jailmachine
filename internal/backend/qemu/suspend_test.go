package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// shortDir is a machine directory short enough for the QMP socket path.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jmsus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// installSuspendFake is fakeQEMUInstall with qemu-system-aarch64 a symlink
// to the test binary rather than a shell shim, so that the process's argv
// names the binary and isOurQEMU recognises it, as for a real QEMU. The
// machine has a disk and firmware variables and a short directory.
func installSuspendFake(t *testing.T, mode string) (*machine.Machine, Paths, string) {
	t.Helper()
	fakeQEMUInstall(t, mode)
	bin, err := exec.LookPath(Binary)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	m := sampleMachine()
	m.Dir = shortDir(t)
	p := Backend{}.paths(m)
	fw, err := FirmwareDir(bin)
	if err != nil {
		t.Fatal(err)
	}
	p.Code = filepath.Join(fw, FirmwareCode)
	for f, data := range map[string]string{p.Disk: "disk", p.Vars: "fw"} {
		if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeRecordEnv, filepath.Join(t.TempDir(), "record.jsonl"))
	t.Cleanup(func() { killRecorded(t) })
	return m, p, bin
}

// killRecorded kills every fake that recorded a command and is still alive.
func killRecorded(t *testing.T) {
	seen := map[int]bool{}
	for _, r := range records(t) {
		if !seen[r.PID] {
			seen[r.PID] = true
			killPID(t, r.PID)
		}
	}
}

// records reads what the fakes of this test received.
func records(t *testing.T) []fakeRecord {
	t.Helper()
	f, err := os.Open(os.Getenv(fakeRecordEnv))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []fakeRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r fakeRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad record %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func commands(recs []fakeRecord) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Cmd)
	}
	return out
}

func findRecord(t *testing.T, recs []fakeRecord, cmd string) (int, fakeRecord) {
	t.Helper()
	for i, r := range recs {
		if r.Cmd == cmd {
			return i, r
		}
	}
	t.Fatalf("the fake never received %s: %q", cmd, commands(recs))
	return -1, fakeRecord{}
}

var userNet = backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222}

// launchFake starts the fake QEMU with the machine's argv (plus -incoming
// defer), records that argv in qemu.argv and waits until QMP answers.
func launchFake(t *testing.T, m *machine.Machine, p Paths, bin string, incoming bool) int {
	t.Helper()
	argv := append([]string{bin}, Args(m, userNet, p)...)
	if incoming {
		argv = append(argv, "-incoming", "defer")
	}
	if err := writeArgv(filepath.Join(m.Dir, ArgvFile), argv); err != nil {
		t.Fatal(err)
	}
	pid, err := procx.StartDetached(bin, argv[1:], nil, p.Log, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(t, pid) })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, err := readPID(p.PID); err == nil && got == pid {
			if q, err := DialMonitor(context.Background(), p.QMP); err == nil {
				q.Close()
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake QEMU never answered: %s", tailOf(p.Log))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeJournalFixture writes a journal for the machine as launched with
// argv, pinned to virt-11.1, with fingerprints of the current disk and
// firmware variables. A "saved" journal gets a valid image of imageBytes.
func writeJournalFixture(t *testing.T, m *machine.Machine, argv []string, phase backend.SuspendPhase, imageBytes int64) *Journal {
	t.Helper()
	sp := suspendPaths(m.Dir)
	pinned, err := withMachineType(argv, "virt-11.1")
	if err != nil {
		t.Fatal(err)
	}
	j := &Journal{Phase: phase, Reason: "test", Argv: pinned, MachineType: "virt-11.1", QEMUBinary: argv[0], Hardware: hardwareOf(m)}
	if phase == backend.SuspendSaved {
		f, err := os.OpenFile(sp.Image, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(imageBytes); err != nil {
			t.Fatal(err)
		}
		f.Close()
		j.ImageBytes, j.SavedAt = imageBytes, time.Now().UTC()
		if j.Fingerprints, err = fingerprintsOf(m.Dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJournal(sp.Journal, j); err != nil {
		t.Fatal(err)
	}
	return j
}

// suspendedFixture is a machine suspended by hand: a saved journal for the
// argv a Start would have used, and a valid image. No QEMU runs.
func suspendedFixture(t *testing.T, mode string) (*machine.Machine, Paths, *Journal) {
	t.Helper()
	m, p, bin := installSuspendFake(t, mode)
	argv := append([]string{bin}, Args(m, userNet, p)...)
	j := writeJournalFixture(t, m, argv, backend.SuspendSaved, 1<<20)
	return m, p, j
}

func stateOf(t *testing.T, m *machine.Machine) backend.State {
	t.Helper()
	st, err := Backend{}.State(m)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func mustNotExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, f := range paths {
		if _, err := os.Lstat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists (%v)", f, err)
		}
	}
}

func mustExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, f := range paths {
		if _, err := os.Lstat(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func journalPhase(t *testing.T, dir string) string {
	t.Helper()
	j, err := readJournal(suspendPaths(dir).Journal)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ""
	case err != nil:
		return "bad"
	}
	return string(j.Phase)
}

// ---- state ----

func TestStateFromFilesTable(t *testing.T) {
	const ourPID = 111
	ours := func(pid int) bool { return pid == ourPID }
	for _, tc := range []struct {
		name    string
		pid     string // "" absent
		journal string // "", "saving", "saved", "bad"
		image   string // "", "valid", "short"
		want    backend.State
	}{
		{"live, no journal", "111", "", "", backend.Running},
		{"live, saved", "111", "saved", "valid", backend.Running},
		{"live, saving", "111", "saving", "", backend.Running},
		{"live, bad journal", "111", "bad", "", backend.Running},
		{"absent, saved, valid", "", "saved", "valid", backend.Suspended},
		{"stale, saved, valid", "222", "saved", "valid", backend.Suspended},
		{"unparseable pid, saved, valid", "garbage", "saved", "valid", backend.Suspended},
		{"absent, saved, short", "", "saved", "short", backend.Broken},
		{"absent, saved, missing", "", "saved", "", backend.Broken},
		{"absent, saving", "", "saving", "", backend.Broken},
		{"stale, saving", "222", "saving", "", backend.Broken},
		{"absent, bad journal", "", "bad", "valid", backend.Broken},
		{"absent, no journal, orphan image", "", "", "valid", backend.Stopped},
		{"absent, nothing", "", "", "", backend.Stopped},
		{"stale, no journal", "222", "", "", backend.Broken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, PIDFile)
			sp := suspendPaths(dir)
			if tc.pid != "" {
				if err := os.WriteFile(pidFile, []byte(tc.pid+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.journal {
			case "saving", "saved":
				j := &Journal{Phase: backend.SuspendPhase(tc.journal), ImageBytes: 4096}
				if err := writeJournal(sp.Journal, j); err != nil {
					t.Fatal(err)
				}
			case "bad":
				if err := os.WriteFile(sp.Journal, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.image {
			case "valid":
				if err := os.WriteFile(sp.Image, make([]byte, 4096), 0o600); err != nil {
					t.Fatal(err)
				}
			case "short":
				if err := os.WriteFile(sp.Image, make([]byte, 100), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := stateFromFiles(pidFile, sp.Journal, sp.Image, ours)
			if err != nil || got != tc.want {
				t.Fatalf("state = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}

func TestRepairKeepsValidSavedPair(t *testing.T) {
	m, p, _ := suspendedFixture(t, "ready")
	sp := suspendPaths(m.Dir)
	for _, f := range []string{p.PID, p.QMP} {
		if err := os.WriteFile(f, []byte("2147483646\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(sp.Tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).Repair(m); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, p.PID, p.QMP, sp.Tmp)
	mustExist(t, sp.Journal, sp.Image)
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state after repair = %s, want suspended", st)
	}
	// Stop on a suspended machine repairs, and repairing keeps the pair.
	if err := (Backend{}).Stop(context.Background(), m, false); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state after Stop = %s, want suspended", st)
	}
}

func TestRepairDiscardsInvalidPair(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, sp journalPaths)
	}{
		{"saved, short image", func(t *testing.T, sp journalPaths) {
			if err := os.Truncate(sp.Image, 10); err != nil {
				t.Fatal(err)
			}
		}},
		{"saved, image missing", func(t *testing.T, sp journalPaths) {
			if err := os.Remove(sp.Image); err != nil {
				t.Fatal(err)
			}
		}},
		{"saving", func(t *testing.T, sp journalPaths) {
			if err := writeJournal(sp.Journal, &Journal{Phase: backend.SuspendSaving}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sp.Tmp, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, p, _ := suspendedFixture(t, "ready")
			sp := suspendPaths(m.Dir)
			tc.prep(t, sp)
			if err := os.WriteFile(p.PID, []byte("2147483646\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if st := stateOf(t, m); st != backend.Broken {
				t.Fatalf("state = %s, want broken", st)
			}
			if err := (Backend{}).Repair(m); err != nil {
				t.Fatal(err)
			}
			mustNotExist(t, sp.Journal, sp.Image, sp.Tmp, p.PID)
			if st := stateOf(t, m); st != backend.Stopped {
				t.Fatalf("state after repair = %s, want stopped", st)
			}
		})
	}
}

func TestRepairRenamesBadJournal(t *testing.T) {
	m, _, _ := suspendedFixture(t, "ready")
	sp := suspendPaths(m.Dir)
	if err := os.WriteFile(sp.Journal, []byte(`{"version":1,"phase":"sav`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Broken {
		t.Fatalf("state = %s, want broken", st)
	}
	if err := (Backend{}).Repair(m); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(sp.Bad); err != nil || string(data) != `{"version":1,"phase":"sav` {
		t.Fatalf("suspend.json.bad = %q, %v", data, err)
	}
	mustNotExist(t, sp.Journal, sp.Image)
	if st := stateOf(t, m); st != backend.Stopped {
		t.Fatalf("state after repair = %s, want stopped", st)
	}
}

// ---- start ----

func TestStartRefusesSuspended(t *testing.T) {
	m, p, _ := suspendedFixture(t, "ready")
	if err := os.WriteFile(p.QMP, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (Backend{}).Start(context.Background(), m, userNet)
	if !errors.Is(err, backend.ErrSuspended) {
		t.Fatalf("Start on a suspended machine: %v", err)
	}
	mustExist(t, p.QMP, suspendPaths(m.Dir).Journal, suspendPaths(m.Dir).Image)
	mustNotExist(t, filepath.Join(m.Dir, ArgvFile), p.Log, os.Getenv(fakeRecordEnv))
}

// A stale pid file beside a valid saved pair reads as suspended, so Start
// refuses before repairing anything, and the pair survives.
func TestStartRefusesSuspendedAfterRepairingStalePID(t *testing.T) {
	m, p, _ := suspendedFixture(t, "ready")
	if err := os.WriteFile(p.PID, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).Start(context.Background(), m, userNet); !errors.Is(err, backend.ErrSuspended) {
		t.Fatalf("Start: %v", err)
	}
	if err := (Backend{}).Repair(m); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).Start(context.Background(), m, userNet); !errors.Is(err, backend.ErrSuspended) {
		t.Fatalf("Start after repair: %v", err)
	}
	mustExist(t, suspendPaths(m.Dir).Journal, suspendPaths(m.Dir).Image)
	mustNotExist(t, filepath.Join(m.Dir, ArgvFile), os.Getenv(fakeRecordEnv))
}

func TestStartSweepsOrphanImages(t *testing.T) {
	m, _, _ := installSuspendFake(t, "ready")
	sp := suspendPaths(m.Dir)
	for _, f := range []string{sp.Image, sp.Tmp} {
		if err := os.WriteFile(f, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if st := stateOf(t, m); st != backend.Stopped {
		t.Fatalf("state with orphan images = %s, want stopped", st)
	}
	if err := (Backend{}).Start(context.Background(), m, userNet); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, sp.Image, sp.Tmp)
}

// ---- suspendable ----

func TestSuspendableRefusesDaemonizedOrLegacy(t *testing.T) {
	var b Backend
	m := sampleMachine()
	m.Dir = t.TempDir()
	if got := b.Suspendable(m); got != legacyHypervisorReason {
		t.Fatalf("no qemu.argv: %q", got)
	}
	argv := append([]string{Binary}, Args(m, userNet, samplePaths(m.Dir))...)
	if err := writeArgv(filepath.Join(m.Dir, ArgvFile), append(slices.Clone(argv), "-daemonize")); err != nil {
		t.Fatal(err)
	}
	if got := b.Suspendable(m); got != legacyHypervisorReason {
		t.Fatalf("argv with -daemonize: %q", got)
	}
	if err := writeArgv(filepath.Join(m.Dir, ArgvFile), argv); err != nil {
		t.Fatal(err)
	}
	if got := b.Suspendable(m); got != "" {
		t.Fatalf("a clean argv: %q", got)
	}

	// The live process is checked too: a qemu.argv can outlive the QEMU
	// it described.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30; :", "-daemonize")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if err := os.WriteFile(filepath.Join(m.Dir, PIDFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := b.Suspendable(m); got != legacyHypervisorReason {
		t.Fatalf("live process with -daemonize: %q", got)
	}

	comma := sampleMachine()
	comma.Dir = filepath.Join(t.TempDir(), "a,b")
	if err := os.MkdirAll(comma.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeArgv(filepath.Join(comma.Dir, ArgvFile), argv); err != nil {
		t.Fatal(err)
	}
	if got := b.Suspendable(comma); !strings.Contains(got, "comma") {
		t.Fatalf("comma in the path: %q", got)
	}
}

// ---- commit ----

// startedMachine boots the fake QEMU through Start and writes a "saving"
// journal through PrepareSuspend.
func startedMachine(t *testing.T) (*machine.Machine, Paths, int) {
	t.Helper()
	m, p, _ := installSuspendFake(t, "ready")
	var b Backend
	ctx := context.Background()
	if err := b.Start(ctx, m, userNet); err != nil {
		t.Fatal(err)
	}
	pid, err := readPID(p.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(t, pid) })
	if st := stateOf(t, m); st != backend.Running {
		t.Fatalf("state after Start = %s", st)
	}
	if reason := b.Suspendable(m); reason != "" {
		t.Fatalf("Suspendable: %s", reason)
	}
	plan := backend.SuspendPlan{Reason: "jm suspend", Meta: map[string]string{"mtu": "9000", "resolver_port": "53123"}}
	if err := b.PrepareSuspend(ctx, m, plan); err != nil {
		t.Fatal(err)
	}
	return m, p, pid
}

// sinceSetup returns the commands from CommitSuspend's first one on, without
// the capabilities handshakes.
func sinceSetup(t *testing.T) []fakeRecord {
	t.Helper()
	recs := records(t)
	i, _ := findRecord(t, recs, "migrate-set-capabilities")
	return slices.DeleteFunc(slices.Clone(recs[i:]), func(r fakeRecord) bool { return r.Cmd == "qmp_capabilities" })
}

func TestCommitSuspendSequence(t *testing.T) {
	m, p, pid := startedMachine(t)
	var b Backend
	sp := suspendPaths(m.Dir)

	j, err := readJournal(sp.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != backend.SuspendSaving || j.MachineType != "virt-11.1" || j.QEMUVersion != "11.1.1" || j.OwnerPID != os.Getpid() {
		t.Fatalf("prepared journal = %+v", j)
	}
	if mt, _ := MachineTypeOf(j.Argv); mt != "virt-11.1" {
		t.Fatalf("journal argv not pinned: %q", j.Argv)
	}
	if !reflect.DeepEqual(j.Hardware, hardwareOf(m)) || j.Meta["mtu"] != "9000" {
		t.Fatalf("journal hardware/meta = %+v / %v", j.Hardware, j.Meta)
	}

	aborted := false
	if err := b.CommitSuspend(context.Background(), m, func() bool { aborted = true; return false }); err != nil {
		t.Fatal(err)
	}
	if !aborted {
		t.Fatal("the abort hook was never consulted")
	}

	recs := sinceSetup(t)
	var got []string
	for _, c := range commands(recs) {
		if c == "query-migrate" && len(got) > 0 && got[len(got)-1] == "query-migrate×n" {
			continue
		}
		if c == "query-migrate" && slices.Contains(got, "migrate") {
			c = "query-migrate×n"
		}
		got = append(got, c)
	}
	want := []string{"migrate-set-capabilities", "migrate-set-parameters", "query-migrate", "stop", "migrate", "query-migrate×n", "quit"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("QMP sequence\n got: %q\nwant: %q", got, want)
	}
	_, stop := findRecord(t, recs, "stop")
	if stop.Journal != "saving" {
		t.Fatalf("journal at stop = %q, want saving", stop.Journal)
	}
	_, mig := findRecord(t, recs, "migrate")
	if string(mig.Args) != `{"uri":"file:`+sp.Tmp+`"}` {
		t.Fatalf("migrate arguments = %s", mig.Args)
	}
	// The commit point: saved, with the image renamed into place, before
	// quit is even sent.
	_, quit := findRecord(t, recs, "quit")
	if quit.Journal != "saved" || !quit.Image || quit.Tmp {
		t.Fatalf("at quit: journal %q, image %v, tmp %v; want saved, true, false", quit.Journal, quit.Image, quit.Tmp)
	}

	if procx.Alive(pid) {
		t.Fatal("QEMU still runs after the commit")
	}
	mustNotExist(t, p.PID, p.QMP, sp.Tmp)
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state = %s, want suspended", st)
	}
	j, err = readJournal(sp.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if j.Fingerprints == nil || j.Fingerprints.Disk.Size != int64(len("disk")) {
		t.Fatalf("fingerprints after exit = %+v", j.Fingerprints)
	}
	if j.ImageBytes != int64(m.MemoryMiB)<<20 || j.Stats == nil || j.Stats.TotalMS != 9213 || j.Stats.DowntimeMS != 19 || j.SavedAt.IsZero() {
		t.Fatalf("saved journal = %+v, stats %+v", j, j.Stats)
	}
	status, err := b.SuspendStatus(m)
	if err != nil || status.Phase != backend.SuspendSaved || status.ImagePath != sp.Image ||
		status.ImageBytes != j.ImageBytes || status.Reason != "jm suspend" || status.Meta["resolver_port"] != "53123" {
		t.Fatalf("SuspendStatus = %+v, %v", status, err)
	}
}

func TestCommitSuspendAbortBeforeStop(t *testing.T) {
	m, _, pid := startedMachine(t)
	err := Backend{}.CommitSuspend(context.Background(), m, func() bool { return true })
	if !errors.Is(err, backend.ErrSuspendAborted) {
		t.Fatalf("err = %v", err)
	}
	cmds := commands(sinceSetup(t))
	if slices.Contains(cmds, "stop") || slices.Contains(cmds, "migrate") {
		t.Fatalf("an aborted commit froze the guest: %q", cmds)
	}
	if !procx.Alive(pid) || journalPhase(t, m.Dir) != "saving" {
		t.Fatal("an abort must leave QEMU running and the journal for CancelSuspend")
	}
	if err := (Backend{}).CancelSuspend(m); err != nil {
		t.Fatal(err)
	}
	if journalPhase(t, m.Dir) != "" || stateOf(t, m) != backend.Running {
		t.Fatal("CancelSuspend did not return the machine to running")
	}
}

func TestCommitSuspendBlockedReasons(t *testing.T) {
	t.Setenv(fakeMigrateEnv, "blocked") // before the launch, so the fake sees it
	m, _, pid := startedMachine(t)
	err := Backend{}.CommitSuspend(context.Background(), m, nil)
	if !errors.Is(err, backend.ErrSuspendBlocked) || !strings.Contains(err.Error(), "VirtFS") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(commands(sinceSetup(t)), "stop") {
		t.Fatal("a blocked migration froze the guest")
	}
	if !procx.Alive(pid) {
		t.Fatal("QEMU was stopped")
	}
}

// monitorStatus reads query-status from the machine's QEMU.
func monitorStatus(t *testing.T, p Paths) string {
	t.Helper()
	q, err := DialMonitor(context.Background(), p.QMP)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	st, err := q.QueryStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCommitSuspendFailedRollsBack(t *testing.T) {
	t.Setenv(fakeMigrateEnv, "fail")
	m, p, pid := startedMachine(t)
	sp := suspendPaths(m.Dir)
	err := Backend{}.CommitSuspend(context.Background(), m, nil)
	if err == nil || !strings.Contains(err.Error(), "No space left on device") || errors.Is(err, backend.ErrSuspendCrashed) {
		t.Fatalf("err = %v", err)
	}
	cmds := commands(sinceSetup(t))
	if i, j := slices.Index(cmds, "migrate"), slices.Index(cmds, "cont"); i < 0 || j < i {
		t.Fatalf("no cont after the failed migration: %q", cmds)
	}
	if slices.Contains(cmds, "quit") {
		t.Fatalf("a failed save quit QEMU: %q", cmds)
	}
	mustNotExist(t, sp.Tmp, sp.Image)
	if journalPhase(t, m.Dir) != "saving" {
		t.Fatal("the journal must stay saving for CancelSuspend")
	}
	if !procx.Alive(pid) || monitorStatus(t, p) != "running" {
		t.Fatal("the guest was not continued")
	}
}

func TestCommitSuspendTimeoutCancels(t *testing.T) {
	t.Setenv(fakeMigrateEnv, "hang")
	saved := suspendMigrateTimeout
	suspendMigrateTimeout = func(int) time.Duration { return 500 * time.Millisecond }
	t.Cleanup(func() { suspendMigrateTimeout = saved })
	m, p, _ := startedMachine(t)
	err := Backend{}.CommitSuspend(context.Background(), m, nil)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v", err)
	}
	cmds := commands(sinceSetup(t))
	if i, j := slices.Index(cmds, "migrate_cancel"), slices.Index(cmds, "cont"); i < 0 || j < i {
		t.Fatalf("want migrate_cancel then cont: %q", cmds)
	}
	mustNotExist(t, suspendPaths(m.Dir).Tmp)
	if journalPhase(t, m.Dir) != "saving" || monitorStatus(t, p) != "running" {
		t.Fatal("a timed-out save must leave the guest running and the journal saving")
	}
}

func TestCommitSuspendShortImageRollsBack(t *testing.T) {
	t.Setenv(fakeMigrateEnv, "short")
	m, p, _ := startedMachine(t)
	err := Backend{}.CommitSuspend(context.Background(), m, nil)
	if err == nil || !strings.Contains(err.Error(), "less than") {
		t.Fatalf("err = %v", err)
	}
	cmds := commands(sinceSetup(t))
	if !slices.Contains(cmds, "cont") || slices.Contains(cmds, "quit") {
		t.Fatalf("commands = %q", cmds)
	}
	mustNotExist(t, suspendPaths(m.Dir).Tmp, suspendPaths(m.Dir).Image)
	if journalPhase(t, m.Dir) != "saving" || monitorStatus(t, p) != "running" {
		t.Fatal("a short image must not be committed")
	}
}

func TestCommitSuspendQEMUDies(t *testing.T) {
	t.Setenv(fakeMigrateEnv, "die")
	m, p, pid := startedMachine(t)
	err := Backend{}.CommitSuspend(context.Background(), m, nil)
	if !errors.Is(err, backend.ErrSuspendCrashed) {
		t.Fatalf("err = %v", err)
	}
	if procx.Alive(pid) {
		t.Fatal("the fake is still alive")
	}
	mustNotExist(t, suspendPaths(m.Dir).Tmp, p.PID)
	// R-C: journal saving with no QEMU reads broken; the caller discards.
	if st := stateOf(t, m); st != backend.Broken {
		t.Fatalf("state = %s, want broken", st)
	}
	if err := (Backend{}).DiscardSuspend(m, "hypervisor exited while suspending"); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Stopped {
		t.Fatalf("state after discard = %s, want stopped", st)
	}
}

// ---- resume ----

func TestSuspendResumeRoundTrip(t *testing.T) {
	m, p, _ := startedMachine(t)
	var b Backend
	ctx := context.Background()
	if err := b.CommitSuspend(ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state = %s, want suspended", st)
	}
	if err := b.Resume(ctx, m, userNet); err != nil {
		t.Fatal(err)
	}
	pid, err := readPID(p.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(t, pid) })
	if st := stateOf(t, m); st != backend.Running {
		t.Fatalf("state after resume = %s, want running", st)
	}
	sp := suspendPaths(m.Dir)
	mustNotExist(t, sp.Journal, sp.Image, sp.Tmp)
	if monitorStatus(t, p) != "running" {
		t.Fatal("the resumed guest is not running")
	}
	// A second suspend of the resumed QEMU (its argv carries -incoming
	// defer and the pinned type) prepares and commits again.
	if err := b.PrepareSuspend(ctx, m, backend.SuspendPlan{Reason: "again"}); err != nil {
		t.Fatal(err)
	}
	j, _ := readJournal(sp.Journal)
	if mt, _ := MachineTypeOf(j.Argv); mt != "virt-11.1" {
		t.Fatalf("second journal argv = %q", j.Argv)
	}
	if err := b.CommitSuspend(ctx, m, nil); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state after the second suspend = %s", st)
	}
}

func TestResumeDeletesJournalBeforeCont(t *testing.T) {
	m, p, j := suspendedFixture(t, "ready")
	sp := suspendPaths(m.Dir)
	if err := os.WriteFile(p.Console, []byte("before the suspend\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state = %s", st)
	}
	if err := (Backend{}).Resume(context.Background(), m, userNet); err != nil {
		t.Fatal(err)
	}
	recs := records(t)
	iCaps, _ := findRecord(t, recs, "migrate-set-capabilities")
	iLoad, load := findRecord(t, recs, "migrate-incoming")
	iCont, cont := findRecord(t, recs, "cont")
	if !(iCaps < iLoad && iLoad < iCont) {
		t.Fatalf("order: %q", commands(recs))
	}
	if load.Journal != "saved" || !load.Image || load.Status != "inmigrate" {
		t.Fatalf("at migrate-incoming: %+v", load)
	}
	if string(load.Args) != `{"uri":"file:`+sp.Image+`"}` {
		t.Fatalf("migrate-incoming arguments = %s", load.Args)
	}
	if cont.Journal != "" || cont.Status != "paused" {
		t.Fatalf("at cont: journal %q, status %q; want no journal and paused", cont.Journal, cont.Status)
	}
	mustNotExist(t, sp.Journal, sp.Image)
	if st := stateOf(t, m); st != backend.Running {
		t.Fatalf("state = %s, want running", st)
	}
	if data, _ := os.ReadFile(p.Console); !strings.Contains(string(data), "before the suspend") {
		t.Fatalf("resume truncated console.log: %q", data)
	}
	argv, err := ReadArgv(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := IncomingArgv(j.Argv, "virt-11.1")
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("qemu.argv\n got: %q\nwant: %q", argv, want)
	}
}

func TestResumeFingerprintMismatchIncompatible(t *testing.T) {
	for _, tc := range []struct {
		name  string
		touch func(p Paths) error
	}{
		{"disk", func(p Paths) error {
			later := time.Now().Add(time.Hour)
			return os.Chtimes(p.Disk, later, later)
		}},
		{"efivars", func(p Paths) error { return os.WriteFile(p.Vars, []byte("changed"), 0o600) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, p, _ := suspendedFixture(t, "ready")
			if err := tc.touch(p); err != nil {
				t.Fatal(err)
			}
			err := Backend{}.Resume(context.Background(), m, userNet)
			if !errors.Is(err, backend.ErrResumeIncompatible) {
				t.Fatalf("err = %v", err)
			}
			mustNotExist(t, filepath.Join(m.Dir, ArgvFile), os.Getenv(fakeRecordEnv))
		})
	}
}

func TestResumeHardwareMismatchIncompatible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, m *machine.Machine)
	}{
		{"cpus", func(_ *testing.T, m *machine.Machine) { m.CPUs = 8 }},
		{"memory", func(_ *testing.T, m *machine.Machine) { m.MemoryMiB = 4096 }},
		{"ssh port", func(_ *testing.T, m *machine.Machine) { m.SSHPort = 2223 }},
		{"shares", func(t *testing.T, m *machine.Machine) { m.Shares = shareMachine(t, t.TempDir()).Shares }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := suspendedFixture(t, "ready")
			tc.change(t, m)
			err := Backend{}.Resume(context.Background(), m, userNet)
			if !errors.Is(err, backend.ErrResumeIncompatible) {
				t.Fatalf("err = %v", err)
			}
			mustNotExist(t, os.Getenv(fakeRecordEnv))
		})
	}
}

func TestResumeLoadFailureIncompatible(t *testing.T) {
	for _, mode := range []string{"fail", "exit"} {
		t.Run(mode, func(t *testing.T) {
			m, p, _ := suspendedFixture(t, "ready")
			t.Setenv(fakeLoadEnv, mode)
			err := Backend{}.Resume(context.Background(), m, userNet)
			if !errors.Is(err, backend.ErrResumeIncompatible) {
				t.Fatalf("err = %v", err)
			}
			data, rerr := os.ReadFile(filepath.Join(m.Dir, ResumeFailedLogFile))
			if rerr != nil {
				t.Fatalf("qemu.resume-failed.log: %v", rerr)
			}
			if mode == "exit" && !strings.Contains(string(data), "load of migration failed") {
				t.Fatalf("qemu.resume-failed.log = %q", data)
			}
			_, load := findRecord(t, records(t), "migrate-incoming")
			if procx.Alive(load.PID) {
				t.Fatal("the rejecting QEMU was left running")
			}
			if slices.Contains(commands(records(t)), "cont") {
				t.Fatal("a rejected load was continued")
			}
			mustNotExist(t, p.PID, p.QMP)
			// Resume never discards; the caller does, on this error.
			if st := stateOf(t, m); st != backend.Suspended {
				t.Fatalf("state = %s, want suspended", st)
			}
		})
	}
}

func TestResumeLaunchFailureTransientKeepsState(t *testing.T) {
	check := func(t *testing.T, m *machine.Machine, err error, want string) {
		t.Helper()
		if err == nil || errors.Is(err, backend.ErrResumeIncompatible) || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v; want a transient error mentioning %q", err, want)
		}
		for _, r := range records(t) {
			if procx.Alive(r.PID) {
				t.Fatalf("QEMU %d left running", r.PID)
			}
		}
		if st := stateOf(t, m); st != backend.Suspended {
			t.Fatalf("state = %s, want suspended", st)
		}
		mustNotExist(t, filepath.Join(m.Dir, ResumeFailedLogFile))
	}
	t.Run("missing binary", func(t *testing.T) {
		m, _, j := suspendedFixture(t, "ready")
		j.Argv[0] = filepath.Join(t.TempDir(), Binary)
		if err := writeJournal(suspendPaths(m.Dir).Journal, j); err != nil {
			t.Fatal(err)
		}
		check(t, m, Backend{}.Resume(context.Background(), m, userNet), "no such file")
	})
	t.Run("exit before QMP", func(t *testing.T) {
		m, _, _ := suspendedFixture(t, "exit")
		check(t, m, Backend{}.Resume(context.Background(), m, userNet), "boom: fake failure")
	})
	t.Run("launch timeout", func(t *testing.T) {
		m, _, _ := suspendedFixture(t, "hang")
		saved := resumeLaunchTimeout
		resumeLaunchTimeout = 500 * time.Millisecond
		t.Cleanup(func() { resumeLaunchTimeout = saved })
		selfFile := filepath.Join(t.TempDir(), "hang.pid")
		t.Setenv(fakeQEMUSelfEnv, selfFile)
		check(t, m, Backend{}.Resume(context.Background(), m, userNet), "not waiting")
	})
	t.Run("load timeout", func(t *testing.T) {
		m, _, _ := suspendedFixture(t, "ready")
		t.Setenv(fakeLoadEnv, "hang")
		saved := resumeLoadTimeout
		resumeLoadTimeout = 500 * time.Millisecond
		t.Cleanup(func() { resumeLoadTimeout = saved })
		check(t, m, Backend{}.Resume(context.Background(), m, userNet), "did not load")
	})
}

func TestResumeUnsupportedMachineTypeIncompatible(t *testing.T) {
	m, _, _ := suspendedFixture(t, "unsupported")
	err := Backend{}.Resume(context.Background(), m, userNet)
	if !errors.Is(err, backend.ErrResumeIncompatible) || !strings.Contains(err.Error(), "unsupported machine type") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(m.Dir, ResumeFailedLogFile)); !strings.Contains(string(data), "unsupported machine type") {
		t.Fatalf("qemu.resume-failed.log = %q", data)
	}
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state = %s, want suspended", st)
	}
}

// The device model comes from the saved argv: environment overrides that
// shape Args are never read on the resume path.
func TestResumeUsesSavedArgvNotEnv(t *testing.T) {
	t.Setenv(AccelEnv, "hvf")
	t.Setenv("JM_9P_SECURITY", "mapped-xattr")
	m, p, bin := installSuspendFake(t, "ready")
	m.Shares = shareMachine(t, t.TempDir()).Shares
	if err := writeShareTable(p.GuestConf, m.Shares); err != nil {
		t.Fatal(err)
	}
	saved := append([]string{bin}, Args(m, userNet, p)...)
	j := writeJournalFixture(t, m, saved, backend.SuspendSaved, 1<<20)

	t.Setenv(AccelEnv, "tcg")
	t.Setenv("JM_9P_SECURITY", "none")
	if err := (Backend{}).Resume(context.Background(), m, userNet); err != nil {
		t.Fatal(err)
	}
	got, err := ReadArgv(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := IncomingArgv(j.Argv, "virt-11.1")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("qemu.argv\n got: %q\nwant: %q", got, want)
	}
	line := strings.Join(got, " ")
	if !strings.Contains(line, "virt-11.1,accel=hvf") || !strings.Contains(line, "security_model=mapped-xattr") ||
		strings.Contains(line, "tcg") || strings.Contains(line, "security_model=none") {
		t.Fatalf("resume argv follows the environment: %s", line)
	}
}

// ---- discard and cancel ----

func TestDiscardRemovesJournalFirst(t *testing.T) {
	m := sampleMachine()
	m.Dir = t.TempDir()
	sp := suspendPaths(m.Dir)
	if err := writeJournal(sp.Journal, &Journal{Phase: backend.SuspendSaved, ImageBytes: 1}); err != nil {
		t.Fatal(err)
	}
	// An image that cannot be removed: a non-empty directory.
	if err := os.MkdirAll(filepath.Join(sp.Image, "stuck"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).DiscardSuspend(m, "test"); err == nil {
		t.Fatal("a failed image removal must be reported")
	}
	mustNotExist(t, sp.Journal)
	mustExist(t, sp.Image)

	if err := os.RemoveAll(sp.Image); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(sp.Journal, &Journal{Phase: backend.SuspendSaved, ImageBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp.Image, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).DiscardSuspend(m, "disk.raw changed"); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, sp.Journal, sp.Image)
	if data, _ := os.ReadFile(filepath.Join(m.Dir, LogFile)); !strings.Contains(string(data), "discarded the saved state: disk.raw changed") {
		t.Fatalf("qemu.log = %q", data)
	}
}

func TestCancelSuspendRefusesSaved(t *testing.T) {
	m := sampleMachine()
	m.Dir = t.TempDir()
	sp := suspendPaths(m.Dir)
	if err := writeJournal(sp.Journal, &Journal{Phase: backend.SuspendSaved, ImageBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := (Backend{}).CancelSuspend(m); err == nil {
		t.Fatal("a committed suspend must not be cancelled")
	}
	mustExist(t, sp.Journal)
	if err := writeJournal(sp.Journal, &Journal{Phase: backend.SuspendSaving}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{sp.Tmp, sp.Image} {
		if err := os.WriteFile(f, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Backend{}).CancelSuspend(m); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, sp.Journal, sp.Tmp, sp.Image)
	if err := (Backend{}).CancelSuspend(m); err != nil {
		t.Fatalf("CancelSuspend with no journal: %v", err)
	}
}

// ---- recovery ----

func TestRecoverTable(t *testing.T) {
	type row struct {
		name           string
		launch         bool
		stalePID       bool
		incoming       bool
		status, mig    string
		journal        string // "", "saving", "saved", "bad"
		noFingerprints bool
		tmp, orphan    bool
		want           backend.RecoverAction
		cont, quit     bool
		alive          bool
		journalAfter   string
		imageAfter     bool
		badAfter       bool
		noQMP          bool // Recover must not talk to QMP
	}
	rows := []row{
		{name: "dead QEMU, saving", journal: "saving", tmp: true, want: backend.RecoverDiscarded},
		{name: "stale pid, saved", stalePID: true, journal: "saved", want: backend.RecoverSuspended, journalAfter: "saved", imageAfter: true},
		{name: "stale pid, saving", stalePID: true, journal: "saving", tmp: true, want: backend.RecoverDiscarded},
		{name: "alive, saving, running", launch: true, journal: "saving", tmp: true,
			want: backend.RecoverResumedGuest, alive: true},
		{name: "alive, saving, paused while migrating", launch: true, status: "paused", mig: "active", journal: "saving", tmp: true,
			want: backend.RecoverResumedGuest, cont: true, alive: true},
		{name: "alive, saving, postmigrate", launch: true, status: "postmigrate", mig: "completed", journal: "saving", tmp: true,
			want: backend.RecoverResumedGuest, cont: true, alive: true},
		{name: "alive, saved, postmigrate (killed between commit and quit)", launch: true, status: "postmigrate", mig: "completed",
			journal: "saved", noFingerprints: true, want: backend.RecoverSuspended, quit: true, journalAfter: "saved", imageAfter: true},
		{name: "alive -incoming, saved, inmigrate (interrupted wake)", launch: true, incoming: true, journal: "saved",
			want: backend.RecoverSuspended, journalAfter: "saved", imageAfter: true},
		{name: "alive -incoming, saved, paused (interrupted wake)", launch: true, incoming: true, status: "paused", mig: "completed",
			journal: "saved", want: backend.RecoverSuspended, journalAfter: "saved", imageAfter: true},
		{name: "alive, saved, running (invariant violated)", launch: true, journal: "saved",
			want: backend.RecoverResumedGuest, alive: true},
		{name: "alive -incoming, no journal, paused", launch: true, incoming: true, status: "paused", mig: "completed", orphan: true,
			want: backend.RecoverResumedGuest, cont: true, alive: true},
		{name: "alive -incoming, no journal, inmigrate", launch: true, incoming: true, orphan: true, want: backend.RecoverDiscarded},
		{name: "alive -incoming, no journal, running", launch: true, incoming: true, status: "running", orphan: true,
			want: backend.RecoverNone, alive: true},
		{name: "alive, no journal", launch: true, orphan: true, want: backend.RecoverNone, alive: true, noQMP: true},
		{name: "dead, no journal", want: backend.RecoverNone, noQMP: true},
		{name: "alive, bad journal, paused", launch: true, status: "paused", journal: "bad", tmp: true,
			want: backend.RecoverResumedGuest, cont: true, alive: true, badAfter: true},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			m, p, bin := installSuspendFake(t, "ready")
			t.Setenv(fakeStatusEnv, tc.status)
			t.Setenv(fakeMigEnv, tc.mig)
			sp := suspendPaths(m.Dir)
			pid := 0
			if tc.launch {
				pid = launchFake(t, m, p, bin, tc.incoming)
			}
			if tc.stalePID {
				if err := os.WriteFile(p.PID, []byte("2147483646\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			argv := append([]string{bin}, Args(m, userNet, p)...)
			switch tc.journal {
			case "saving", "saved":
				j := writeJournalFixture(t, m, argv, backend.SuspendPhase(tc.journal), 1<<20)
				if tc.noFingerprints {
					j.Fingerprints = nil
					if err := writeJournal(sp.Journal, j); err != nil {
						t.Fatal(err)
					}
				}
			case "bad":
				if err := os.WriteFile(sp.Journal, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.tmp {
				if err := os.WriteFile(sp.Tmp, []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.orphan {
				if err := os.WriteFile(sp.Image, []byte("orphan"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := len(records(t))

			got, err := Backend{}.Recover(context.Background(), m)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("action = %s, want %s", got, tc.want)
			}
			recs := records(t)[before:]
			cmds := commands(recs)
			if tc.noQMP && len(recs) != 0 {
				t.Fatalf("Recover talked to QMP: %q", cmds)
			}
			if slices.Contains(cmds, "cont") != tc.cont {
				t.Fatalf("cont sent = %v, want %v: %q", !tc.cont, tc.cont, cmds)
			}
			if tc.quit && !slices.Contains(cmds, "quit") {
				t.Fatalf("no quit: %q", cmds)
			}
			if tc.launch {
				if procx.Alive(pid) != tc.alive {
					t.Fatalf("QEMU alive = %v, want %v", !tc.alive, tc.alive)
				}
				if tc.alive && monitorStatus(t, p) != "running" {
					t.Fatal("a recovered guest is not running")
				}
				if !tc.alive {
					mustNotExist(t, p.PID, p.QMP)
				}
			}
			if ph := journalPhase(t, m.Dir); ph != tc.journalAfter {
				t.Fatalf("journal after = %q, want %q", ph, tc.journalAfter)
			}
			if _, err := os.Stat(sp.Image); (err == nil) != tc.imageAfter {
				t.Fatalf("image exists = %v, want %v", err == nil, tc.imageAfter)
			}
			if _, err := os.Stat(sp.Bad); (err == nil) != tc.badAfter {
				t.Fatalf("suspend.json.bad exists = %v, want %v", err == nil, tc.badAfter)
			}
			mustNotExist(t, sp.Tmp)
			if tc.journalAfter == "saved" {
				j, _ := readJournal(sp.Journal)
				if j.Fingerprints == nil {
					t.Fatal("a recovered suspended machine has no fingerprints")
				}
				if st := stateOf(t, m); st != backend.Suspended {
					t.Fatalf("state = %s, want suspended", st)
				}
			}
		})
	}
}

// Load-bearing (ADR 0009, D6): once the journal says "saved", the image is the
// machine, and whatever state the live QEMU is in, recovery never continues it.
func TestRecoverNeverContsSavedJournal(t *testing.T) {
	for _, status := range []string{"postmigrate", "paused", "inmigrate", "prelaunch", "shutdown", "running", "no QMP"} {
		for _, incoming := range []bool{false, true} {
			t.Run(status+"/incoming="+strconv.FormatBool(incoming), func(t *testing.T) {
				saved := quitTimeout
				quitTimeout = time.Second
				t.Cleanup(func() { quitTimeout = saved })
				m, p, bin := installSuspendFake(t, "ready")
				if status != "no QMP" {
					t.Setenv(fakeStatusEnv, status)
				}
				t.Setenv(fakeMigEnv, "completed")
				pid := launchFake(t, m, p, bin, incoming)
				if status == "no QMP" {
					if err := os.Remove(p.QMP); err != nil {
						t.Fatal(err)
					}
				}
				argv := append([]string{bin}, Args(m, userNet, p)...)
				writeJournalFixture(t, m, argv, backend.SuspendSaved, 1<<20)
				before := len(records(t))

				got, err := Backend{}.Recover(context.Background(), m)
				if err != nil {
					t.Fatal(err)
				}
				if cmds := commands(records(t)[before:]); slices.Contains(cmds, "cont") {
					t.Fatalf("recovery sent cont to a saved journal's QEMU: %q", cmds)
				}
				if status == "running" {
					// The invariant was already broken: the running guest
					// wins and the stale image goes.
					if got != backend.RecoverResumedGuest || journalPhase(t, m.Dir) != "" || !procx.Alive(pid) {
						t.Fatalf("action %s, journal %q, alive %v", got, journalPhase(t, m.Dir), procx.Alive(pid))
					}
					return
				}
				if got != backend.RecoverSuspended {
					t.Fatalf("action = %s, want suspended", got)
				}
				if procx.Alive(pid) {
					t.Fatal("QEMU was left running beside a saved journal")
				}
				if st := stateOf(t, m); st != backend.Suspended {
					t.Fatalf("state = %s, want suspended", st)
				}
			})
		}
	}
}

// Every woken QEMU keeps -incoming in its argv. With no journal, a monitor
// that does not answer is not an interrupted transition, and must not keep a
// caller from stopping or removing the machine.
func TestRecoverIncomingNoJournalQMPUnreachable(t *testing.T) {
	m, p, bin := installSuspendFake(t, "ready")
	pid := launchFake(t, m, p, bin, true)
	if err := os.Remove(p.QMP); err != nil {
		t.Fatal(err)
	}
	got, err := Backend{}.Recover(context.Background(), m)
	if err != nil || got != backend.RecoverNone {
		t.Fatalf("Recover = %s, %v; want none, nil", got, err)
	}
	if !procx.Alive(pid) {
		t.Fatal("Recover killed a running QEMU")
	}
}

// A QEMU that survives SIGKILL after a failed resume stays recorded, so the
// machine reads running and no second QEMU is launched on the same disk; the
// error is never "incompatible", so nothing is discarded or booted.
func TestResumeKillSurvivorStaysRecorded(t *testing.T) {
	for _, load := range []string{"hang", "fail"} {
		t.Run(load, func(t *testing.T) {
			m, p, _ := suspendedFixture(t, "ready")
			t.Setenv(fakeLoadEnv, load)
			saved, savedKill := resumeLoadTimeout, killAndWaitFn
			resumeLoadTimeout = 500 * time.Millisecond
			killAndWaitFn = func(int) bool { return false }
			t.Cleanup(func() { resumeLoadTimeout, killAndWaitFn = saved, savedKill })
			t.Cleanup(func() {
				for _, r := range records(t) {
					killPID(t, r.PID)
				}
			})

			err := Backend{}.Resume(context.Background(), m, userNet)
			if err == nil || errors.Is(err, backend.ErrResumeIncompatible) || !strings.Contains(err.Error(), "did not exit after SIGKILL") {
				t.Fatalf("err = %v", err)
			}
			mustExist(t, p.PID, suspendPaths(m.Dir).Journal, suspendPaths(m.Dir).Image)
			if st := stateOf(t, m); st != backend.Running {
				t.Fatalf("state = %s, want running (recorded survivor)", st)
			}
		})
	}
}

// A journal or image that cannot be read is not an invalid one: repair,
// recovery and cancel change nothing and report the error.
func TestUnreadableSavedStateIsNeverDiscarded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of their mode")
	}
	m, _, _ := suspendedFixture(t, "ready")
	sp := suspendPaths(m.Dir)
	if err := os.Chmod(sp.Journal, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sp.Journal, 0o600) })

	if err := (Backend{}).Repair(m); err == nil {
		t.Error("Repair succeeded on an unreadable journal")
	}
	if _, err := (Backend{}).Recover(context.Background(), m); err == nil {
		t.Error("Recover succeeded on an unreadable journal")
	}
	if err := (Backend{}).CancelSuspend(m); err == nil {
		t.Error("CancelSuspend succeeded on an unreadable journal")
	}
	mustExist(t, sp.Journal, sp.Image)
	mustNotExist(t, sp.Bad)
	if err := os.Chmod(sp.Journal, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := stateOf(t, m); st != backend.Suspended {
		t.Fatalf("state = %s, want suspended", st)
	}

	// An image that cannot be examined is not "missing".
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	j := &Journal{Phase: backend.SuspendSaved, ImageBytes: 1}
	if ok, err := checkImage(filepath.Join(locked, "suspend.state"), j); ok || err == nil {
		t.Errorf("checkImage = %v, %v; want an error", ok, err)
	}
	if ok, err := checkImage(filepath.Join(t.TempDir(), "missing"), j); ok || err != nil {
		t.Errorf("checkImage on a missing image = %v, %v; want false, nil", ok, err)
	}
}
