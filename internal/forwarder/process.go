package forwarder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The forwarder runs as a detached "jm _forwarder <name>" (own session,
// pid file, log file) so it outlives the "jm start" that launched it, and
// is recognised by pid plus argv like the hypervisor and gvproxy: pid files
// survive reboots and pids are recycled (ADR 0005).

// Command is the hidden subcommand name.
const Command = "_forwarder"

// Timeouts for Stop.
const (
	termTimeout  = 5 * time.Second
	pollInterval = 100 * time.Millisecond
)

// Process locates one machine's forwarder.
type Process struct {
	Dir  string // machine directory
	Name string // machine name (appears in argv)
	Root string // state root (appears in argv)
}

func (p Process) pidFile() string { return filepath.Join(p.Dir, PIDFile) }

// LogPath is the forwarder's log file.
func (p Process) LogPath() string { return filepath.Join(p.Dir, LogFile) }

// Args returns the argument vector (without argv[0]) the forwarder is
// launched with; Alive matches a live process against it.
func (p Process) Args() []string {
	return []string{"--state-root", p.Root, Command, p.Name}
}

// Alive reports whether the pid in forwarder.pid is a live forwarder for
// this machine, and its pid.
func (p Process) Alive() (int, bool) {
	pid, err := readPID(p.pidFile())
	if err != nil {
		return 0, false
	}
	return pid, isOurs(commandLine(pid), p)
}

// isOurs matches argv against the substrings that identify this machine's
// forwarder: "--state-root <root>" and "<subcommand> <name>", as the
// hypervisor and gvproxy checks do. Substring matching keeps a state root
// containing spaces recognisable (ps prints argv joined by spaces).
func isOurs(argv string, p Process) bool {
	argv = " " + argv + " "
	return strings.Contains(argv, " --state-root "+p.Root+" ") &&
		strings.Contains(argv, " "+Command+" "+p.Name+" ")
}

// Start launches the forwarder detached unless one is already alive. exe
// is the jm binary (os.Executable of the caller).
func (p Process) Start(exe string) error {
	if _, ok := p.Alive(); ok {
		return nil
	}
	_ = os.Remove(p.pidFile())
	logf, err := os.OpenFile(p.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("forwarder: opening %s: %w", p.LogPath(), err)
	}
	defer logf.Close()
	// Not CommandContext: the forwarder must outlive this jm invocation.
	cmd := exec.Command(exe, p.Args()...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("forwarder: failed to start: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap in the background so an early exit does not leave a zombie.
	go func() { _ = cmd.Wait() }()
	if err := os.WriteFile(p.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return fmt.Errorf("forwarder: writing %s: %w", p.pidFile(), err)
	}
	return nil
}

// Stop terminates a live forwarder (SIGTERM, wait, SIGKILL) and removes the
// pid file. A dead or absent one is just tidied away. Signals go to the
// forwarder's process group (it is a session leader, so pgid == pid) so
// that children still in the group go with it.
func (p Process) Stop(ctx context.Context) error {
	if pid, ok := p.Alive(); ok {
		if err := signalGroup(pid, syscall.SIGTERM); err != nil && processAlive(pid) {
			return fmt.Errorf("forwarder: SIGTERM pid %d: %w", pid, err)
		}
		if !waitExit(ctx, pid, termTimeout) {
			_ = signalGroup(pid, syscall.SIGKILL)
			if !waitExit(ctx, pid, termTimeout) {
				return fmt.Errorf("forwarder: pid %d did not exit after SIGKILL", pid)
			}
		}
	}
	if err := os.Remove(p.pidFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// signalGroup sends sig to pid's process group, falling back to pid alone
// when it is not a group leader.
func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err == nil {
		return nil
	}
	return syscall.Kill(pid, sig)
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("forwarder: bad pid file %s: %q", path, strings.TrimSpace(string(data)))
	}
	return pid, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// commandLine returns the argv of pid as reported by ps, "" if none.
func commandLine(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

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
