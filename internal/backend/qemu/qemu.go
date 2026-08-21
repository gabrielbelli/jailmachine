// Package qemu implements backend.Backend with QEMU (HVF on macOS, KVM on
// Linux later): -M virt, EDK2 pflash, virtio-blk/net/rng, serial console to
// console.log, QMP socket for graceful power-down.
//
// The backend finds a machine's files through Machine.Dir, which the machine
// store fills in on load (ADR 0005); it knows nothing about the state root.
package qemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// Name is the identifier stored in Machine.Backend.
const Name = "qemu"

// Timeouts used by Stop.
const (
	gracefulTimeout = 30 * time.Second
	termTimeout     = 5 * time.Second
	pollInterval    = 200 * time.Millisecond
)

// ErrRunning is returned by Start when the machine is already running.
var ErrRunning = errors.New("qemu: machine is already running")

// ErrNoDir is returned when a Machine was not loaded through the store and
// so has no directory.
var ErrNoDir = errors.New("qemu: machine record has no directory (load it through the machine store)")

// Backend is the QEMU implementation of backend.Backend.
type Backend struct{}

func init() { backend.Register(Backend{}) }

// Name implements backend.Backend.
func (Backend) Name() string { return Name }

// Capabilities implements backend.Backend: a serial console, and host
// filesystem sharing over virtio-9p (ADR 0007).
func (Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{SerialConsole: true, FileSharing: true}
}

// Preflight implements backend.Backend: the emulator and its firmware must
// be on the host.
func (Backend) Preflight() error {
	bin, err := LookupBinary()
	if err != nil {
		return err
	}
	_, err = FirmwareDir(bin)
	return err
}

// ConsolePath implements backend.Backend.
func (b Backend) ConsolePath(m *machine.Machine) string {
	if m.Dir == "" {
		return ""
	}
	return filepath.Join(m.Dir, machine.ConsoleFile)
}

// Logs implements backend.Backend: qemu's own log first, then the guest's
// serial console.
func (b Backend) Logs(m *machine.Machine) []string {
	if m.Dir == "" {
		return nil
	}
	p := b.paths(m)
	return []string{p.Log, p.Console}
}

func (b Backend) paths(m *machine.Machine) Paths {
	dir := m.Dir
	return Paths{
		Vars:      filepath.Join(dir, machine.EFIVarsFile),
		Disk:      filepath.Join(dir, machine.DiskFile),
		Seed:      filepath.Join(dir, machine.SeedFile),
		Console:   filepath.Join(dir, machine.ConsoleFile),
		QMP:       QMPSocket(dir),
		PID:       filepath.Join(dir, PIDFile),
		Log:       filepath.Join(dir, LogFile),
		GuestConf: filepath.Join(dir, machine.GuestConfDir),
	}
}

// State implements backend.Backend: computed from the pid file and the
// process behind it, never cached. A pid that is alive but is not our QEMU
// (pid recycled after a reboot or crash) is Broken, so Start repairs and
// Stop never signals a foreign process.
func (b Backend) State(m *machine.Machine) (backend.State, error) {
	if m.Dir == "" {
		return "", ErrNoDir
	}
	pidFile := b.paths(m).PID
	return stateFromPIDFile(pidFile, func(pid int) bool { return isOurQEMU(pid, pidFile) })
}

// stateFromPIDFile is the pure core of State, shared with tests. ours
// decides whether a live pid is really the QEMU we started.
func stateFromPIDFile(pidFile string, ours func(pid int) bool) (backend.State, error) {
	pid, err := readPID(pidFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return backend.Stopped, nil
	case err != nil:
		// Unparseable pid file: the process cannot be found, so treat the
		// machine as broken rather than failing the caller.
		return backend.Broken, nil
	}
	if ours(pid) {
		return backend.Running, nil
	}
	return backend.Broken, nil
}

// Repair removes stale runtime files left behind by a dead QEMU (ADR 0005
// "broken" -> "stopped").
func (b Backend) Repair(m *machine.Machine) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	p := b.paths(m)
	return removeAll(p.PID, p.QMP)
}

func removeAll(paths ...string) error {
	var errs []error
	for _, f := range paths {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Start implements backend.Backend: daemonises qemu-system-aarch64 with the
// PoC argv and returns once the pid file exists.
func (b Backend) Start(ctx context.Context, m *machine.Machine, net backend.NetAttachment) error {
	st, err := b.State(m)
	if err != nil {
		return err
	}
	switch st {
	case backend.Running:
		return ErrRunning
	case backend.Broken:
		if err := b.Repair(m); err != nil {
			return fmt.Errorf("qemu: repairing stale state: %w", err)
		}
	}

	p := b.paths(m)
	if _, err := os.Stat(p.Disk); err != nil {
		return fmt.Errorf("qemu: disk image missing (run 'jm init'): %w", err)
	}
	bin, err := LookupBinary()
	if err != nil {
		return err
	}
	fw, err := FirmwareDir(bin)
	if err != nil {
		return err
	}
	p.Code = filepath.Join(fw, FirmwareCode)
	if err := ensureEFIVars(filepath.Join(fw, FirmwareVars), p.Vars); err != nil {
		return err
	}
	if len(p.QMP) > MaxSocketPath {
		return fmt.Errorf("qemu: QMP socket path %q is %d bytes; unix sockets are limited to %d (use a shorter --state-root or $TMPDIR)", p.QMP, len(p.QMP), MaxSocketPath)
	}
	if err := removeAll(p.QMP); err != nil {
		return fmt.Errorf("qemu: removing stale QMP socket: %w", err)
	}

	// Host filesystem sharing (ADR 0007): a share whose host path has
	// vanished since it was added (an unplugged disk) is dropped rather
	// than allowed to keep the machine from booting. The record is left
	// alone — the directory may be back next time — so the filtering
	// happens on a copy.
	run := *m
	run.Shares, _ = machine.UsableShares(m.Shares)
	if err := writeShareTable(p.GuestConf, run.Shares); err != nil {
		return err
	}

	args := Args(&run, net, p)
	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("qemu: opening %s: %w", p.Log, err)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	runErr := cmd.Run()
	_ = logf.Close()
	if runErr != nil {
		return fmt.Errorf("qemu: failed to start (%v): %s", runErr, tailOf(p.Log))
	}
	// With -daemonize qemu only exits 0 once the child has written the pid
	// file, but be defensive: a missing pid file means nothing is running.
	if _, err := readPID(p.PID); err != nil {
		return fmt.Errorf("qemu: exited without writing %s: %s", p.PID, tailOf(p.Log))
	}
	return nil
}

// writeShareTable publishes the share table into the directory exported to
// the guest as machine.GuestConfTag, so that the guest's boot-time mount
// script knows which mount tag belongs at which path. The table is written
// even when it is empty, so that a machine whose shares were all removed
// does not keep mounting yesterday's set.
func writeShareTable(dir string, shares []machine.Share) error {
	if dir == "" {
		return nil
	}
	if len(shares) == 0 {
		// Nothing to say and nothing said before: do not litter the
		// machine directory on machines that share nothing.
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("qemu: creating %s: %w", dir, err)
	}
	tab := filepath.Join(dir, machine.SharesTabFile)
	if err := os.WriteFile(tab, []byte(machine.SharesTab(shares)), 0o644); err != nil {
		return fmt.Errorf("qemu: writing %s: %w", tab, err)
	}
	return nil
}

// ensureEFIVars copies the pristine EDK2 variable store into the machine
// directory the first time the machine boots. The copy is written to a
// temporary sibling and renamed into place, so an interrupted copy never
// leaves a truncated store behind; a store whose size differs from the
// template (a truncated copy, or a template from a different QEMU build) is
// treated as absent and replaced.
func ensureEFIVars(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("qemu: reading firmware vars template: %w", err)
	}
	if st, err := os.Stat(dst); err == nil && st.Size() == int64(len(data)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".*.tmp")
	if err != nil {
		return fmt.Errorf("qemu: writing %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("qemu: writing %s: %w", dst, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("qemu: writing %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("qemu: writing %s: %w", dst, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("qemu: writing %s: %w", dst, err)
	}
	return nil
}

// tailOf returns the trimmed contents of a (small) log file, or a hint
// when it cannot be read.
func tailOf(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "(no output in " + path + ")"
	}
	const max = 4096
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return strings.TrimSpace(string(data))
}

// Stop implements backend.Backend. graceful asks the guest to power down
// over QMP and waits up to 30 s before escalating to SIGTERM then SIGKILL;
// graceful=false sends SIGTERM straight away. The pid and QMP files are
// removed only once the process is confirmed gone, so a Stop that fails
// leaves State() reporting Running rather than Stopped.
func (b Backend) Stop(ctx context.Context, m *machine.Machine, graceful bool) error {
	p := b.paths(m)
	st, err := b.State(m)
	if err != nil {
		return err
	}
	if st != backend.Running {
		// Stopped: nothing to do. Broken: tidy up so the next State is Stopped.
		return b.Repair(m)
	}
	pid, err := readPID(p.PID)
	if err != nil {
		return err
	}
	if err := b.shutdown(ctx, pid, p.QMP, graceful); err != nil {
		return err
	}
	return removeAll(p.PID, p.QMP)
}

// ResizeDisk implements backend.Resizer via QMP block_resize on the root
// virtio disk.
func (b Backend) ResizeDisk(ctx context.Context, m *machine.Machine, size int64) error {
	return BlockResize(ctx, QMPSocket(m.Dir), DiskDevice, size)
}

// shutdown drives pid to exit: optionally QMP powerdown, then SIGTERM, then
// SIGKILL. The CLI may already have asked the guest to power off over SSH,
// so the graceful wait runs even when QMP itself fails.
func (b Backend) shutdown(ctx context.Context, pid int, qmp string, graceful bool) error {
	if graceful {
		_ = Powerdown(ctx, qmp)
		if waitExit(ctx, pid, gracefulTimeout) {
			return nil
		}
	}
	if err := terminate(pid); err != nil && !processAlive(pid) {
		return nil
	}
	if waitExit(ctx, pid, termTimeout) {
		return nil
	}
	if err := kill(pid); err != nil && !processAlive(pid) {
		return nil
	}
	if waitExit(ctx, pid, termTimeout) {
		return nil
	}
	return fmt.Errorf("qemu: pid %d did not exit after SIGKILL", pid)
}

// Cleanup implements backend.Cleaner: it removes the QMP socket when
// QMPSocket placed it outside the machine directory (see QMPSocket), which
// deleting the directory would otherwise leave behind. In-tree files are
// left to the directory removal.
func (b Backend) Cleanup(m *machine.Machine) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	qmp := QMPSocket(m.Dir)
	if backend.InTree(m.Dir, qmp) {
		return nil
	}
	return removeAll(qmp)
}

// waitExit polls until the process is gone, the timeout lapses or ctx is
// cancelled. It returns true if the process exited.
func waitExit(ctx context.Context, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return !processAlive(pid)
		case <-time.After(pollInterval):
		}
	}
}
