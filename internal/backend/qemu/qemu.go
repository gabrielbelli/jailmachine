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

// Capabilities implements backend.Backend: QEMU gives us a serial console
// and nothing else yet.
func (Backend) Capabilities() backend.Capabilities {
	return backend.Capabilities{SerialConsole: true}
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
		Vars:    filepath.Join(dir, machine.EFIVarsFile),
		Disk:    filepath.Join(dir, machine.DiskFile),
		Seed:    filepath.Join(dir, machine.SeedFile),
		Console: filepath.Join(dir, machine.ConsoleFile),
		QMP:     QMPSocket(dir),
		PID:     filepath.Join(dir, PIDFile),
		Log:     filepath.Join(dir, LogFile),
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

	args := Args(m, net, p)
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

// ensureEFIVars copies the pristine EDK2 variable store into the machine
// directory the first time the machine boots.
func ensureEFIVars(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("qemu: reading firmware vars template: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
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
// graceful=false sends SIGTERM straight away. Stale pid/QMP files are always
// removed.
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
	defer func() { _ = removeAll(p.PID, p.QMP) }()

	if graceful {
		if err := Powerdown(ctx, p.QMP); err == nil {
			if waitExit(ctx, pid, gracefulTimeout) {
				return nil
			}
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
