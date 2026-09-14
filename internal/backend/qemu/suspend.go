package qemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// Suspend is QEMU migration to a file (ADR 0009): stop, migrate with
// mapped-ram and multifd to suspend.state.tmp, make it durable, rename it,
// commit the journal as "saved", then quit. Resume launches the saved argv
// with -incoming defer, loads the file, removes the journal and only then
// continues the guest. The one rule everything else rests on: a guest whose
// journal says "saved" is never continued, because from the commit on the
// saved image is the machine.

var _ backend.Suspender = Backend{}

// MultifdChannels is the number of parallel channels a save and a load use.
const MultifdChannels = 4

// ResumeFailedLogFile is machine.ResumeFailedLogFile, where qemu.log is
// copied when QEMU rejects a saved state.
const ResumeFailedLogFile = machine.ResumeFailedLogFile

// legacyHypervisorReason is what Suspendable says about a QEMU started by a
// jm that still daemonised it: migrating such a process aborts under HVF.
const legacyHypervisorReason = "hypervisor was started by an older jm; restart it once: jm stop && jm start"

// Timeouts and poll intervals. Variables so tests can shorten them.
var (
	migratePollInterval  = 200 * time.Millisecond
	migrateCancelTimeout = 30 * time.Second
	quitTimeout          = 10 * time.Second
	statusPollInterval   = 50 * time.Millisecond
	resumeLaunchTimeout  = 10 * time.Second
	resumeLoadTimeout    = 2 * time.Minute
	resumeContTimeout    = 5 * time.Second
	// suspendMigrateTimeout bounds a save: max(60 s, 30 s per GiB).
	suspendMigrateTimeout = func(memoryMiB int) time.Duration {
		return max(60*time.Second, time.Duration(memoryMiB)*30*time.Second/1024)
	}
)

// migrationActive reports whether a query-migrate status is one QEMU is
// still working through.
func migrationActive(status string) bool {
	switch status {
	case "", "none", "completed", "failed", "cancelled":
		return false
	}
	return true
}

// Suspendable implements backend.Suspender from files and the process table
// only.
func (b Backend) Suspendable(m *machine.Machine) string {
	if m.Dir == "" {
		return ErrNoDir.Error()
	}
	if strings.Contains(m.Dir, ",") {
		return "the machine directory path contains a comma, which a migration file URI cannot carry"
	}
	argv, err := ReadArgv(m.Dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return legacyHypervisorReason
	case err != nil:
		return err.Error()
	case slices.Contains(argv, "-daemonize"):
		return legacyHypervisorReason
	}
	if pid, err := readPID(b.paths(m).PID); err == nil {
		if strings.Contains(" "+commandLine(pid)+" ", " -daemonize ") {
			return legacyHypervisorReason
		}
	}
	return ""
}

// SuspendStatus implements backend.Suspender from the journal alone.
func (b Backend) SuspendStatus(m *machine.Machine) (backend.SuspendStatus, error) {
	if m.Dir == "" {
		return backend.SuspendStatus{}, ErrNoDir
	}
	sp := suspendPaths(m.Dir)
	j, err := readJournal(sp.Journal)
	if errors.Is(err, os.ErrNotExist) {
		return backend.SuspendStatus{}, nil
	}
	if err != nil {
		return backend.SuspendStatus{}, err
	}
	return backend.SuspendStatus{
		Phase:          j.Phase,
		Reason:         j.Reason,
		SavedAt:        j.SavedAt,
		ImagePath:      sp.Image,
		ImageBytes:     j.ImageBytes,
		AllocatedBytes: j.ImageAllocated,
		Meta:           maps.Clone(j.Meta),
	}, nil
}

func unavailable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", backend.ErrSuspendUnavailable, fmt.Sprintf(format, args...))
}

// PrepareSuspend implements backend.Suspender: checks over QMP that the
// running QEMU can save to a file (S3), then writes a "saving" journal with
// the argv pinned to a versioned machine type (S4). The guest is untouched.
func (b Backend) PrepareSuspend(ctx context.Context, m *machine.Machine, plan backend.SuspendPlan) error {
	if reason := b.Suspendable(m); reason != "" {
		return unavailable("%s", reason)
	}
	st, err := b.State(m)
	if err != nil {
		return err
	}
	if st != backend.Running {
		return unavailable("machine is %s, not running", st)
	}
	p, sp := b.paths(m), suspendPaths(m.Dir)
	if _, err := os.Lstat(sp.Journal); err == nil {
		return unavailable("a suspend is already in progress (%s exists)", sp.Journal)
	}
	argv, err := ReadArgv(m.Dir)
	if err != nil || len(argv) == 0 {
		return unavailable("reading %s: %v", ArgvFile, err)
	}
	mon, err := DialMonitor(ctx, p.QMP)
	if err != nil {
		return unavailable("%v", err)
	}
	defer mon.Close()
	status, err := mon.QueryStatus(ctx)
	if err != nil {
		return unavailable("%v", err)
	}
	if status != "running" {
		return unavailable("the guest is %s, not running", status)
	}
	if err := mon.FileMigrationCapable(ctx); err != nil {
		return unavailable("%v", err)
	}
	version, err := mon.QueryVersion(ctx)
	if err != nil {
		return unavailable("%v", err)
	}
	machineType, err := PinMachineType(ctx, argv, mon)
	if err != nil {
		return unavailable("%v", err)
	}
	// blocked-reasons are expected here (shares are still mounted); only
	// the check after the guest has been quiesced is fatal.
	info, err := mon.QueryMigrate(ctx)
	if err != nil {
		return unavailable("%v", err)
	}
	if migrationActive(info.Status) {
		return unavailable("a migration is already %s", info.Status)
	}
	pinned, err := withMachineType(argv, machineType)
	if err != nil {
		return unavailable("%v", err)
	}
	j := &Journal{
		Phase:       backend.SuspendSaving,
		Reason:      plan.Reason,
		OwnerPID:    os.Getpid(),
		StartedAt:   time.Now().UTC(),
		Argv:        pinned,
		MachineType: machineType,
		QEMUVersion: version,
		QEMUBinary:  argv[0],
		Hardware:    hardwareOf(m),
		Meta:        maps.Clone(plan.Meta),
	}
	return writeJournal(sp.Journal, j)
}

// commit carries one CommitSuspend through its steps.
type commit struct {
	b   Backend
	m   *machine.Machine
	p   Paths
	sp  journalPaths
	j   *Journal
	pid int
}

func (c *commit) alive() bool { return isOurQEMU(c.pid, c.p.PID) }

// crashed is R-C: QEMU is gone. The partial image is removed; the "saving"
// journal is left for the caller to discard, and the pid file and QMP socket
// are tidied so the state reads as broken rather than running.
func (c *commit) crashed(cause error) error {
	_ = removeAll(c.sp.Tmp)
	if !procx.Alive(c.pid) {
		_ = removeAll(c.p.PID, c.p.QMP)
	}
	return fmt.Errorf("%w: %v", backend.ErrSuspendCrashed, cause)
}

// beforeFreeze reports a failure while the guest still runs.
func (c *commit) beforeFreeze(err error) error {
	if !c.alive() {
		return c.crashed(err)
	}
	return err
}

// rollback is the QMP half of R-B: the guest is stopped (and maybe
// migrating) but the journal still says "saving". It cancels an active
// migration, removes the partial image, continues the guest and confirms it
// runs. It returns cause, annotated when the rollback itself failed. It dials
// a fresh QMP session, because the one in use may have broken mid-reply.
func (c *commit) rollback(cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrateCancelTimeout+resumeContTimeout+2*qmpTimeout)
	defer cancel()
	if !c.alive() {
		return c.crashed(cause)
	}
	mon, err := DialMonitor(ctx, c.p.QMP)
	if err != nil {
		if !c.alive() {
			return c.crashed(cause)
		}
		return fmt.Errorf("%w; rolling back: %v (the guest may still be paused)", cause, err)
	}
	defer mon.Close()
	if err := cancelMigration(ctx, mon); err != nil {
		return fmt.Errorf("%w; rolling back: %v (the guest is still paused)", cause, err)
	}
	_ = removeImages(c.sp)
	if err := contAndConfirm(ctx, mon); err != nil {
		if !c.alive() {
			return c.crashed(cause)
		}
		return fmt.Errorf("%w; rolling back: %v", cause, err)
	}
	return cause
}

// cancelMigration cancels an active migration and waits until it is not.
func cancelMigration(ctx context.Context, mon *Monitor) error {
	info, err := mon.QueryMigrate(ctx)
	if err != nil {
		return err
	}
	if !migrationActive(info.Status) {
		return nil
	}
	if info.Status != "cancelling" {
		if err := mon.MigrateCancel(ctx); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(migrateCancelTimeout)
	for {
		info, err := mon.QueryMigrate(ctx)
		if err != nil {
			return err
		}
		if !migrationActive(info.Status) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the migration is still %s after migrate_cancel", info.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(migratePollInterval):
		}
	}
}

// contAndConfirm sends cont and waits for query-status to say "running".
func contAndConfirm(ctx context.Context, mon *Monitor) error {
	if err := mon.Cont(ctx); err != nil {
		return err
	}
	return waitStatus(ctx, mon, resumeContTimeout, "running")
}

// waitStatus polls query-status until it is one of want.
func waitStatus(ctx context.Context, mon *Monitor, timeout time.Duration, want ...string) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := mon.QueryStatus(ctx)
		if err != nil {
			return err
		}
		if slices.Contains(want, status) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the guest is %s, not %s, after %s", status, strings.Join(want, " or "), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

// CommitSuspend implements backend.Suspender, steps S10 to S14 and the QMP
// half of the R-B rollback.
//
// Before the commit, a failure leaves the guest running with the journal
// still "saving" (the caller cancels it), or returns ErrSuspendCrashed when
// QEMU has died. The commit point is the journal rewritten as "saved", after
// the image has been synced (F_FULLFSYNC) and renamed into place and before
// QEMU is told to quit; from there on the machine is suspended even if this
// process dies, and the guest is never continued.
func (b Backend) CommitSuspend(ctx context.Context, m *machine.Machine, abort func() bool) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	c := &commit{b: b, m: m, p: b.paths(m), sp: suspendPaths(m.Dir)}
	j, err := readJournal(c.sp.Journal)
	if err != nil {
		return fmt.Errorf("qemu: committing a suspend: %w", err)
	}
	if j.Phase != backend.SuspendSaving {
		return fmt.Errorf("qemu: committing a suspend: the journal is %s, not saving", j.Phase)
	}
	c.j = j
	if c.pid, err = readPID(c.p.PID); err != nil {
		return c.crashed(err)
	}
	if !c.alive() {
		return c.crashed(fmt.Errorf("pid %d is not this machine's QEMU", c.pid))
	}
	mon, err := DialMonitor(ctx, c.p.QMP)
	if err != nil {
		return c.beforeFreeze(unavailable("%v", err))
	}
	defer func() { mon.Close() }()

	// S10: migration setup; a device that blocks migration refuses here.
	if err := mon.SetFileMigration(ctx, MultifdChannels); err != nil {
		return c.beforeFreeze(unavailable("%v", err))
	}
	info, err := mon.QueryMigrate(ctx)
	if err != nil {
		return c.beforeFreeze(unavailable("%v", err))
	}
	if len(info.BlockedReasons) > 0 {
		return fmt.Errorf("%w: blocked: %s", backend.ErrSuspendBlocked, strings.Join(info.BlockedReasons, "; "))
	}

	// S11: the last moment a client can cancel for free.
	if abort != nil && abort() {
		return backend.ErrSuspendAborted
	}
	if err := mon.Stop(ctx); err != nil {
		return c.rollback(fmt.Errorf("qemu: stopping the guest: %w", err))
	}

	// S12: save.
	f, err := os.OpenFile(c.sp.Tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return c.rollback(fmt.Errorf("qemu: creating %s: %w", c.sp.Tmp, err))
	}
	f.Close()
	if err := mon.Migrate(ctx, "file:"+c.sp.Tmp); err != nil {
		return c.rollback(fmt.Errorf("qemu: starting the save: %w", err))
	}
	timeout := suspendMigrateTimeout(j.Hardware.MemoryMiB)
	deadline := time.Now().Add(timeout)
	for {
		info, err = mon.QueryMigrate(ctx)
		if err != nil {
			if !c.alive() {
				return c.crashed(err)
			}
			return c.rollback(fmt.Errorf("qemu: watching the save: %w", err))
		}
		if info.Status == "completed" {
			break
		}
		switch info.Status {
		case "failed":
			return c.rollback(fmt.Errorf("qemu: the save failed: %s", info.ErrorDesc))
		case "cancelled":
			return c.rollback(errors.New("qemu: the save was cancelled"))
		}
		if time.Now().After(deadline) {
			return c.rollback(fmt.Errorf("qemu: the save did not finish within %s", timeout))
		}
		select {
		case <-ctx.Done():
			return c.rollback(fmt.Errorf("qemu: the save was interrupted: %w", ctx.Err()))
		case <-time.After(migratePollInterval):
		}
	}

	// S13: durability, then the commit.
	size, allocated, err := makeDurable(c.sp, j.Hardware.MemoryMiB)
	if err != nil {
		return c.rollback(err)
	}
	j.Phase = backend.SuspendSaved
	j.SavedAt = time.Now().UTC()
	j.ImageBytes = size
	j.ImageAllocated = allocated
	j.Stats = &SuspendStats{TotalMS: info.TotalTime, DowntimeMS: info.Downtime}
	if info.RAM != nil {
		j.Stats.Transferred = info.RAM.Transferred
	}
	if err := writeJournal(c.sp.Journal, j); err != nil {
		cur, rerr := readJournal(c.sp.Journal)
		switch {
		case rerr == nil && cur.Phase == backend.SuspendSaving:
			return c.rollback(err) // the rename never happened
		case rerr == nil && cur.Phase == backend.SuspendSaved:
			// Renamed, and only the directory sync failed: committed.
		default:
			// Unknown: never continue a guest that may be committed.
			// Recovery resolves it under the next lock.
			return fmt.Errorf("qemu: committing the suspend: %w (the guest is left paused for recovery)", err)
		}
	}
	// COMMIT POINT. From here the machine is suspended.

	// S14: end QEMU. Block devices were inactivated when the save completed.
	mon.Close()
	if q, err := DialMonitor(context.Background(), c.p.QMP); err == nil {
		_ = q.Quit(context.Background()) // QEMU may hang up before it replies
		q.Close()
	}
	if !endProcess(c.pid, quitTimeout) {
		return fmt.Errorf("qemu: the machine is suspended, but QEMU (pid %d) did not exit after SIGKILL", c.pid)
	}
	_ = removeAll(c.p.PID, c.p.QMP)
	if fp, err := fingerprintsOf(m.Dir); err == nil {
		j.Fingerprints = fp
		_ = writeJournal(c.sp.Journal, j) // optional: its absence skips the check
	}
	return nil
}

// makeDurable syncs the partial image, checks that it holds at least the
// guest's memory, renames it into place and syncs the directory. It returns
// the image's logical and allocated sizes.
func makeDurable(sp journalPaths, memoryMiB int) (int64, int64, error) {
	f, err := os.OpenFile(sp.Tmp, os.O_RDWR, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("qemu: opening the saved image: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, 0, fmt.Errorf("qemu: syncing the saved image: %w", err)
	}
	st, err := f.Stat()
	f.Close()
	if err != nil {
		return 0, 0, fmt.Errorf("qemu: reading the saved image: %w", err)
	}
	if want := int64(memoryMiB) << 20; st.Size() < want {
		return 0, 0, fmt.Errorf("qemu: the saved image is %d bytes, less than the guest's %d MiB", st.Size(), memoryMiB)
	}
	if err := os.Rename(sp.Tmp, sp.Image); err != nil {
		return 0, 0, fmt.Errorf("qemu: renaming the saved image: %w", err)
	}
	if err := syncDir(filepath.Dir(sp.Image)); err != nil {
		return 0, 0, err
	}
	return st.Size(), allocatedBytes(st), nil
}

// endProcess waits grace for pid to exit, then sends SIGTERM and SIGKILL in
// turn. It reports whether the process is gone.
func endProcess(pid int, grace time.Duration) bool {
	bg := context.Background()
	if procx.WaitExit(bg, pid, grace) {
		return true
	}
	_ = terminate(pid)
	if procx.WaitExit(bg, pid, termTimeout) {
		return true
	}
	return killAndWait(pid)
}

// killAndWait sends SIGKILL and reports whether pid is gone.
func killAndWait(pid int) bool {
	_ = kill(pid)
	return procx.WaitExit(context.Background(), pid, termTimeout)
}

// CancelSuspend implements backend.Suspender: it removes a "saving" journal,
// durably, then any partial or uncommitted image. A "saved" journal is
// refused: it can only be resumed or discarded.
func (b Backend) CancelSuspend(m *machine.Machine) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	sp := suspendPaths(m.Dir)
	j, err := readJournal(sp.Journal)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err == nil && j.Phase == backend.SuspendSaved:
		return errors.New("qemu: the suspend is already committed; resume or discard it")
	case err != nil && !errors.Is(err, errBadJournal):
		return fmt.Errorf("qemu: reading the suspend journal (nothing was changed): %w", err)
	}
	return discardJournalThenImage(sp)
}

// DiscardSuspend implements backend.Suspender: the journal is removed first
// and the directory synced, then the image. The reason is recorded in
// qemu.log.
func (b Backend) DiscardSuspend(m *machine.Machine, reason string) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	if err := discardJournalThenImage(suspendPaths(m.Dir)); err != nil {
		return err
	}
	appendLog(b.paths(m).Log, "discarded the saved state: "+reason)
	return nil
}

// appendLog adds a "jm: " line to a log file, best effort.
func appendLog(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "jm: %s %s\n", time.Now().UTC().Format(time.RFC3339), line)
}

func incompatible(format string, args ...any) error {
	return fmt.Errorf("%w: %s", backend.ErrResumeIncompatible, fmt.Sprintf(format, args...))
}

// Resume implements backend.Suspender: W0 (validation from files), W3
// (launch the saved argv waiting for a migration), W4 (load) and W5 (remove
// the journal, then continue). net is not used: the saved argv already
// attaches the NIC to the provider's socket.
//
// An error wrapping backend.ErrResumeIncompatible means the saved state can
// never be loaded (the caller discards it); any other error before W5 leaves
// QEMU killed and the machine suspended. Once the journal is gone the guest
// is never killed: a failure to continue it is returned as a plain error and
// Recover continues it.
func (b Backend) Resume(ctx context.Context, m *machine.Machine, _ backend.NetAttachment) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	p, sp := b.paths(m), suspendPaths(m.Dir)
	st, err := b.State(m)
	if err != nil {
		return err
	}
	if st == backend.Running {
		return ErrRunning
	}
	j, err := readJournal(sp.Journal)
	if err != nil || j.Phase != backend.SuspendSaved {
		return fmt.Errorf("qemu: machine is not suspended (%s)", st)
	}

	// W0: validate from files.
	if !validImage(sp.Image, j) {
		return incompatible("the saved image %s is missing or not %d bytes", sp.Image, j.ImageBytes)
	}
	if d := hardwareOf(m).diff(j.Hardware); d != "" {
		return incompatible("the machine record changed while suspended: %s", d)
	}
	if j.Fingerprints != nil {
		cur, err := fingerprintsOf(m.Dir)
		switch {
		case err != nil:
			return incompatible("%v", err)
		case cur.Disk != j.Fingerprints.Disk:
			return incompatible("%s changed after the state was saved", machine.DiskFile)
		case cur.EFIVars != j.Fingerprints.EFIVars:
			return incompatible("%s changed after the state was saved", machine.EFIVarsFile)
		}
	}
	if len(j.Argv) < 2 || j.MachineType == "" {
		return incompatible("the journal has no hypervisor command line")
	}
	bin := j.Argv[0]
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("qemu: cannot resume: %w", err)
	}

	// W3: launch the saved argv, waiting for the migration.
	fwDir, _ := FirmwareDir(bin)
	resolved, absent, err := resolveSavedArgv(j.Argv, m.Dir, fwDir)
	if err != nil {
		return err
	}
	argv, err := IncomingArgv(resolved, j.MachineType)
	if err != nil {
		return incompatible("%v", err)
	}
	usable, _ := machine.UsableShares(m.Shares)
	shares := slices.DeleteFunc(usable, func(s machine.Share) bool { return slices.Contains(absent, s.Tag) })
	if err := writeShareTable(p.GuestConf, shares); err != nil {
		return err
	}
	if err := removeAll(p.PID, p.QMP); err != nil {
		return fmt.Errorf("qemu: removing stale runtime files: %w", err)
	}
	if err := writeArgv(filepath.Join(m.Dir, ArgvFile), argv); err != nil {
		return err
	}
	pid, err := procx.StartDetached(argv[0], argv[1:], nil, p.Log, true)
	if err != nil {
		return fmt.Errorf("qemu: failed to start for the resume: %w", err)
	}
	r := &resume{p: p, dir: m.Dir, pid: pid}
	mon, err := r.waitIncoming(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if mon != nil {
			mon.Close()
		}
	}()

	// W4: load.
	if err := mon.SetFileMigration(ctx, MultifdChannels); err != nil {
		return r.loadFailed(err, false)
	}
	if err := mon.MigrateIncoming(ctx, "file:"+sp.Image); err != nil {
		return r.loadFailed(err, true)
	}
	if mon, err = r.waitLoaded(ctx, mon); err != nil {
		return err
	}

	// W5: invalidate, then run. The journal goes first, so a crash from
	// here on can never load this image a second time.
	if err := os.Remove(sp.Journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return r.transient("removing the suspend journal before continuing: %v", err)
	}
	// Until the unlink is durable a crash could bring the journal back
	// beside a guest that has run on, so the guest is not continued. It is
	// left paused and loaded, with no journal: recovery syncs and continues
	// it under the next lock.
	if err := syncDirRetry(m.Dir); err != nil {
		return fmt.Errorf("qemu: making the suspend journal's removal durable: %w (the restored guest is left paused; the next jm start continues it)", err)
	}
	cctx, cancel := context.WithTimeout(context.Background(), resumeContTimeout+2*qmpTimeout)
	defer cancel()
	if err := contAndConfirm(cctx, mon); err != nil {
		return fmt.Errorf("qemu: continuing the restored guest: %w", err)
	}
	_ = removeImages(sp)
	return nil
}

// resume carries one Resume through W3 and W4.
type resume struct {
	p   Paths
	dir string
	pid int
}

// killAndWaitFn is killAndWait; a variable so tests can simulate a process
// that survives SIGKILL.
var killAndWaitFn = killAndWait

// kill ends a QEMU that failed to resume and tidies its runtime files. It
// reports false when the process survived SIGKILL: its pid file is then kept
// (written if QEMU had not written it yet), so the machine reads running
// with its journal and recovery ends it, rather than reading suspended and
// letting a retry launch a second QEMU on the same disk.
func (r *resume) kill() bool {
	if procx.Alive(r.pid) && !killAndWaitFn(r.pid) {
		if got, err := readPID(r.p.PID); err != nil || got != r.pid {
			_ = writeAtomic(r.p.PID, []byte(strconv.Itoa(r.pid)+"\n"), 0o600)
		}
		return false
	}
	_ = removeAll(r.p.PID, r.p.QMP)
	return true
}

// survived is the error for a QEMU that kill could not end. It never wraps
// backend.ErrResumeIncompatible: nothing may be discarded or booted while
// that process still has the disk open.
func (r *resume) survived(cause string) error {
	return fmt.Errorf("qemu: resume: %s; QEMU (pid %d) did not exit after SIGKILL and is still recorded in %s", cause, r.pid, r.p.PID)
}

// keepLog copies qemu.log aside for a rejected load.
func (r *resume) keepLog() {
	src, err := os.Open(r.p.Log)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(r.dir, ResumeFailedLogFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer dst.Close()
	_, _ = io.Copy(dst, src)
}

// rejected kills QEMU, keeps its log and reports an incompatible state.
func (r *resume) rejected(format string, args ...any) error {
	r.keepLog()
	if !r.kill() {
		return r.survived(fmt.Sprintf(format, args...))
	}
	return incompatible(format, args...)
}

// transient kills QEMU and reports a failure that keeps the saved state.
func (r *resume) transient(format string, args ...any) error {
	if !r.kill() {
		return r.survived(fmt.Sprintf(format, args...))
	}
	return fmt.Errorf("qemu: resume: "+format, args...)
}

// waitIncoming polls the launched QEMU until its pid file names it and QMP
// reports "inmigrate".
func (r *resume) waitIncoming(ctx context.Context) (*Monitor, error) {
	deadline := time.Now().Add(resumeLaunchTimeout)
	for {
		if !procx.Alive(r.pid) {
			tail := tailOf(r.p.Log)
			if strings.Contains(strings.ToLower(tail), "unsupported machine type") {
				return nil, r.rejected("%s", tail)
			}
			return nil, r.transient("exited during startup: %s", tail)
		}
		if got, err := readPID(r.p.PID); err == nil && got == r.pid {
			dctx, cancel := context.WithTimeout(ctx, time.Second)
			mon, err := DialMonitor(dctx, r.p.QMP)
			if err == nil {
				status, serr := mon.QueryStatus(dctx)
				if serr == nil && status == "inmigrate" {
					cancel()
					return mon, nil
				}
				mon.Close()
			}
			cancel()
		}
		if time.Now().After(deadline) {
			return nil, r.transient("not waiting for the saved state after %s: %s", resumeLaunchTimeout, tailOf(r.p.Log))
		}
		select {
		case <-ctx.Done():
			return nil, r.transient("%v", ctx.Err())
		case <-time.After(statusPollInterval):
		}
	}
}

// loadFailed classifies a failed load command. An error reply, or QEMU dying
// once migrate-incoming was sent, is a rejection; anything else is transient.
func (r *resume) loadFailed(err error, sent bool) error {
	var qe *QMPError
	if errors.As(err, &qe) {
		return r.rejected("%v", err)
	}
	if sent && !procx.Alive(r.pid) {
		return r.rejected("QEMU exited while loading: %v: %s", err, tailOf(r.p.Log))
	}
	return r.transient("%v", err)
}

// waitLoaded polls until the load has finished ("paused"). A failed
// migration or QEMU exiting is a rejection; the timeout is transient. A
// monitor that stops answering while QEMU lives is redialled.
func (r *resume) waitLoaded(ctx context.Context, mon *Monitor) (*Monitor, error) {
	deadline := time.Now().Add(resumeLoadTimeout)
	for {
		status, err := mon.QueryStatus(ctx)
		if err == nil && (status == "paused" || status == "running") {
			return mon, nil
		}
		if err == nil {
			var info MigrationInfo
			if info, err = mon.QueryMigrate(ctx); err == nil && info.Status == "failed" {
				mon.Close()
				return nil, r.rejected("the saved state was rejected: %s", info.ErrorDesc)
			}
		}
		if err != nil {
			mon.Close()
			if !procx.Alive(r.pid) {
				return nil, r.rejected("QEMU exited while loading: %s", tailOf(r.p.Log))
			}
			if ctx.Err() != nil {
				return nil, r.transient("%v", ctx.Err())
			}
			if mon, err = DialMonitor(ctx, r.p.QMP); err != nil {
				if !procx.Alive(r.pid) {
					return nil, r.rejected("QEMU exited while loading: %s", tailOf(r.p.Log))
				}
				return nil, r.transient("%v", err)
			}
		}
		if time.Now().After(deadline) {
			mon.Close()
			return nil, r.transient("the saved state did not load within %s", resumeLoadTimeout)
		}
		select {
		case <-ctx.Done():
			mon.Close()
			return nil, r.transient("%v", ctx.Err())
		case <-time.After(statusPollInterval):
		}
	}
}

// Recover implements backend.Suspender: it resolves a suspend or resume that
// a killed process left half done (ADR 0009 recovery table). It talks to QMP
// only when a journal exists or the running QEMU was launched with
// -incoming. It never sends cont to a QEMU whose journal says "saved".
//
//	QEMU (ours)       journal   QMP status             action -> result
//	alive             saving    running                discard -> resumed guest
//	alive             saving    paused, postmigrate    cancel, cont, discard -> resumed guest
//	alive             saved     postmigrate            quit (kill), fingerprint -> suspended
//	alive             saved     anything else          kill -> suspended
//	alive             saved     running                invariant violated: discard -> resumed guest
//	alive, -incoming  none      paused                 cont -> resumed guest
//	alive, -incoming  none      inmigrate              kill, sweep -> discarded
//	none              saving    -                      discard -> discarded
//	none              saved     -                      keep a valid pair -> suspended
//
// An unparseable journal with a live QEMU is set aside and treated as
// "saving": its image can never be loaded, so continuing the guest is safe.
func (b Backend) Recover(ctx context.Context, m *machine.Machine) (backend.RecoverAction, error) {
	if m.Dir == "" {
		return backend.RecoverNone, ErrNoDir
	}
	p, sp := b.paths(m), suspendPaths(m.Dir)
	j, jerr := readJournal(sp.Journal)
	noJournal := errors.Is(jerr, os.ErrNotExist)
	bad := errors.Is(jerr, errBadJournal)
	if jerr != nil && !noJournal && !bad {
		// Unreadable is not invalid: decide nothing on it.
		return backend.RecoverNone, fmt.Errorf("qemu: reading the suspend journal (nothing was changed): %w", jerr)
	}
	pid, perr := readPID(p.PID)
	alive := perr == nil && isOurQEMU(pid, p.PID)

	if !alive {
		if noJournal {
			return backend.RecoverNone, nil
		}
		if err := b.Repair(m); err != nil {
			return backend.RecoverNone, err
		}
		if !bad && j.Phase == backend.SuspendSaved && validImage(sp.Image, j) {
			return backend.RecoverSuspended, nil
		}
		return backend.RecoverDiscarded, nil
	}
	if !noJournal && !bad && j.Phase == backend.SuspendSaved {
		return b.recoverSaved(ctx, m, p, sp, j, pid)
	}
	if noJournal {
		// No suspend journal: only a QEMU started for a resume can be
		// mid-transition. Anything else is a machine running normally,
		// and an image beside it is an orphan.
		if argv, err := ReadArgv(m.Dir); err != nil || !hasIncoming(argv) {
			_ = removeImages(sp)
			return backend.RecoverNone, nil
		}
	}
	mon, err := DialMonitor(ctx, p.QMP)
	if err == nil {
		defer mon.Close()
	}
	status := ""
	if err == nil {
		status, err = settledStatus(ctx, mon)
	}
	if err != nil {
		if noJournal {
			// Only a journal makes a live guest's state ambiguous. Every
			// QEMU that was ever woken keeps -incoming in its argv, so a
			// hung monitor here must not stop the caller from stopping,
			// killing or removing it; the running stages report a dead
			// monitor themselves.
			appendLog(p.Log, "recovery skipped: the monitor of a resumed QEMU does not answer: "+err.Error())
			return backend.RecoverNone, nil
		}
		return backend.RecoverNone, fmt.Errorf("qemu: recovering an interrupted transition: %w", err)
	}
	if noJournal {
		return b.recoverIncoming(ctx, p, sp, mon, pid, status)
	}
	if bad {
		if err := os.Rename(sp.Journal, sp.Bad); err != nil && !errors.Is(err, os.ErrNotExist) {
			return backend.RecoverNone, fmt.Errorf("qemu: setting aside a bad suspend journal: %w", err)
		}
		_ = syncDir(m.Dir)
	}
	return b.recoverSaving(ctx, p, sp, mon, pid, status)
}

// settledStatus reads query-status, waiting out the brief "finish-migrate".
func settledStatus(ctx context.Context, mon *Monitor) (string, error) {
	deadline := time.Now().Add(qmpTimeout)
	for {
		status, err := mon.QueryStatus(ctx)
		if err != nil || status != "finish-migrate" || time.Now().After(deadline) {
			return status, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(statusPollInterval):
		}
	}
}

// recoverSaved handles a live QEMU with a committed journal. There is no
// cont anywhere in this function, and it must stay that way: the saved image
// is the machine, and a continued guest would diverge from it.
func (b Backend) recoverSaved(ctx context.Context, m *machine.Machine, p Paths, sp journalPaths, j *Journal, pid int) (backend.RecoverAction, error) {
	status := ""
	mon, err := DialMonitor(ctx, p.QMP)
	if err == nil {
		status, _ = settledStatus(ctx, mon)
	}
	grace := time.Duration(0)
	switch status {
	case "running":
		mon.Close()
		// Impossible under the invariants (the journal goes before cont):
		// the image is stale, the running guest is the truth.
		if err := b.DiscardSuspend(m, "invariant violated: the guest was running with a saved journal"); err != nil {
			return backend.RecoverNone, err
		}
		return backend.RecoverResumedGuest, nil
	case "postmigrate":
		// Killed between the commit and quit.
		_ = mon.Quit(ctx)
		grace = quitTimeout
	}
	if mon != nil {
		mon.Close()
	}
	if !endProcessKill(pid, grace) {
		return backend.RecoverNone, fmt.Errorf("qemu: recovering a suspended machine: pid %d did not exit after SIGKILL", pid)
	}
	if err := removeAll(p.PID, p.QMP); err != nil {
		return backend.RecoverNone, err
	}
	if j.Fingerprints == nil {
		if fp, err := fingerprintsOf(m.Dir); err == nil {
			j.Fingerprints = fp
			_ = writeJournal(sp.Journal, j)
		}
	}
	if err := repairSuspend(sp); err != nil {
		return backend.RecoverNone, err
	}
	if validImage(sp.Image, j) {
		return backend.RecoverSuspended, nil
	}
	return backend.RecoverDiscarded, nil
}

// syncDirRetry is syncDir tried a few times, for the one sync a guest's
// continuation waits on.
func syncDirRetry(dir string) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = syncDir(dir); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// endProcessKill waits grace for pid to exit, then SIGKILLs it.
func endProcessKill(pid int, grace time.Duration) bool {
	if grace > 0 && procx.WaitExit(context.Background(), pid, grace) {
		return true
	}
	return killAndWait(pid)
}

// recoverSaving handles a live QEMU with an uncommitted journal: the guest is
// continued if it was frozen, and the journal and any image are discarded.
func (b Backend) recoverSaving(ctx context.Context, p Paths, sp journalPaths, mon *Monitor, pid int, status string) (backend.RecoverAction, error) {
	switch status {
	case "running":
	case "paused", "postmigrate":
		if err := cancelMigration(ctx, mon); err != nil {
			return backend.RecoverNone, fmt.Errorf("qemu: recovering an interrupted suspend: %w", err)
		}
		_ = removeAll(sp.Tmp)
		if err := contAndConfirm(ctx, mon); err != nil {
			return backend.RecoverNone, fmt.Errorf("qemu: recovering an interrupted suspend: %w", err)
		}
	case "inmigrate":
		if !killAndWait(pid) {
			return backend.RecoverNone, fmt.Errorf("qemu: recovering: pid %d did not exit after SIGKILL", pid)
		}
		if err := removeAll(p.PID, p.QMP); err != nil {
			return backend.RecoverNone, err
		}
		if err := discardJournalThenImage(sp); err != nil {
			return backend.RecoverNone, err
		}
		return backend.RecoverDiscarded, nil
	default:
		return backend.RecoverNone, fmt.Errorf("qemu: cannot recover an interrupted suspend: the guest is %s", status)
	}
	if err := discardJournalThenImage(sp); err != nil {
		return backend.RecoverNone, err
	}
	return backend.RecoverResumedGuest, nil
}

// recoverIncoming handles a live QEMU launched for a resume with no journal
// left: the journal is removed only after the load, so a paused guest is
// complete and is continued; one still waiting for a migration never loaded
// and is killed.
func (b Backend) recoverIncoming(ctx context.Context, p Paths, sp journalPaths, mon *Monitor, pid int, status string) (backend.RecoverAction, error) {
	switch status {
	case "paused":
		// The wake may have died because the unlink could not be synced.
		if err := syncDirRetry(filepath.Dir(sp.Journal)); err != nil {
			return backend.RecoverNone, fmt.Errorf("qemu: recovering an interrupted resume: %w", err)
		}
		if err := contAndConfirm(ctx, mon); err != nil {
			return backend.RecoverNone, fmt.Errorf("qemu: recovering an interrupted resume: %w", err)
		}
		_ = removeImages(sp)
		return backend.RecoverResumedGuest, nil
	case "inmigrate":
		if !killAndWait(pid) {
			return backend.RecoverNone, fmt.Errorf("qemu: recovering: pid %d did not exit after SIGKILL", pid)
		}
		if err := removeAll(p.PID, p.QMP); err != nil {
			return backend.RecoverNone, err
		}
		if err := removeImages(sp); err != nil {
			return backend.RecoverNone, err
		}
		return backend.RecoverDiscarded, nil
	}
	_ = removeImages(sp)
	return backend.RecoverNone, nil
}
