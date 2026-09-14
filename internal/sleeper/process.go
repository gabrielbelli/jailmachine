// Package sleeper is the per-machine helper of ADR 0009: "jm _sleeper <name>"
// runs a suspend, and while its machine is suspended it holds the host
// endpoints clients were given (the engine socket and the SSH port). A
// connection that arrives there starts a wake in a detached "jm _wake <name>"
// child, and is relayed byte for byte once the real endpoints are back.
//
// This package holds the mechanics, free of any command-line policy: the
// process handling (this file), the stand-in (standin.go), the control
// protocol (control.go) and the status file (status.go). The mode loop and
// the control handlers live in the CLI.
package sleeper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// The sleeper runs as a detached "jm --state-root <root> _sleeper <name>"
// (launched through procx, with a pid file and a log file) and is recognised
// by pid plus argv, like the forwarder and the resolver: pid files survive
// reboots and pids are recycled (ADR 0005).
const (
	// Command is the hidden subcommand of the sleeper.
	Command = "_sleeper"
	// WakeCommand is the hidden subcommand the sleeper spawns to wake its
	// machine for a held connection.
	WakeCommand = "_wake"
)

// Files kept in the machine directory. The control socket goes through
// backend.SocketPath, so it may fall back to the temp dir (see Cleanup).
const (
	PIDFile     = "sleeper.pid"
	LogFile     = "sleeper.log"
	ControlFile = "sleeper.sock"
)

// Timeouts. Variables so tests can shorten them.
var (
	termTimeout  = 5 * time.Second
	readyTimeout = 5 * time.Second
	pollInterval = 50 * time.Millisecond
)

// commandLineOf is commandLine, replaced in tests.
var commandLineOf = commandLine

// Process locates one machine's sleeper.
type Process struct {
	Dir  string // machine directory
	Name string // machine name (appears in argv)
	Root string // state root (appears in argv)
	// KeepEnv names JM_* variables the sleeper and its children keep:
	// where a program is, not how jm behaves.
	KeepEnv []string
}

func (p Process) pidFile() string { return filepath.Join(p.Dir, PIDFile) }

// LogPath is the sleeper's log file.
func (p Process) LogPath() string { return filepath.Join(p.Dir, LogFile) }

// StatusPath is the status file the sleeper rewrites on every change.
func (p Process) StatusPath() string { return filepath.Join(p.Dir, machine.SleeperStatusFile) }

// WakeLogPath is where "_wake" children log.
func (p Process) WakeLogPath() string { return filepath.Join(p.Dir, machine.WakeLogFile) }

// ControlPath is the sleeper's control socket.
func (p Process) ControlPath() string { return backend.SocketPath(p.Dir, ControlFile) }

// Args returns the argument vector (without argv[0]) the sleeper is launched
// with; Alive matches a live process against it.
func (p Process) Args() []string {
	return []string{"--state-root", p.Root, Command, p.Name}
}

// WakeArgs is the argument vector of a "_wake" child.
func (p Process) WakeArgs() []string {
	return []string{"--state-root", p.Root, WakeCommand, p.Name}
}

// Alive reports whether the pid in sleeper.pid is a live sleeper for this
// machine, and its pid.
func (p Process) Alive() (int, bool) {
	pid, err := readPID(p.pidFile())
	if err != nil {
		return 0, false
	}
	return pid, IsOurs(commandLineOf(pid), p)
}

// IsOurs matches argv against the substrings that identify this machine's
// sleeper: "--state-root <root>" and "_sleeper <name>". Substring matching
// keeps a state root containing spaces recognisable (ps prints argv joined
// by spaces).
func IsOurs(argv string, p Process) bool {
	argv = " " + argv + " "
	return strings.Contains(argv, " --state-root "+p.Root+" ") &&
		strings.Contains(argv, " "+Command+" "+p.Name+" ")
}

// ChildEnv is environ without any JM_* variable except those named in keep.
// The sleeper and its "_wake" children are detached helpers: the environment
// of the shell that happened to start them must never change what they do
// (ADR 0009). The state root travels in argv instead. keep is for variables
// that only locate a program, which a wake needs as much as a start does.
func ChildEnv(environ []string, keep ...string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(kv, "JM_") && !slices.Contains(keep, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Start launches the sleeper detached unless one is already alive, and waits
// until its control socket answers. exe is the jm binary. The sleeper writes
// its own pid file once it holds the pid file lock (LockPID), so two racing
// launches leave exactly one sleeper and a pid file that names it.
func (p Process) Start(ctx context.Context, exe string) error {
	if _, ok := p.Alive(); ok && Answers(p.ControlPath()) {
		return nil
	}
	pid, err := procx.StartDetached(exe, p.Args(), ChildEnv(os.Environ(), p.KeepEnv...), p.LogPath(), false)
	if err != nil {
		return fmt.Errorf("sleeper: failed to start: %w", err)
	}
	deadline := time.Now().Add(readyTimeout)
	for {
		if Answers(p.ControlPath()) {
			return nil
		}
		if !procx.Alive(pid) {
			if Answers(p.ControlPath()) {
				return nil // another sleeper won the race
			}
			return fmt.Errorf("sleeper: exited before answering on %s: %s", p.ControlPath(), tail(p.LogPath()))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sleeper: timed out waiting for %s: %s", p.ControlPath(), tail(p.LogPath()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Stop terminates a live sleeper (SIGTERM to its process group, wait,
// SIGKILL) and removes its pid file. A dead or absent one is just tidied
// away. Stop refuses to signal this process and never signals its caller's
// process group (procx.SignalGroup).
func (p Process) Stop(ctx context.Context) error {
	if pid, ok := p.Alive(); ok {
		if err := procx.SignalGroup(pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, procx.ErrSelf) {
				return fmt.Errorf("sleeper: %s names this process (pid %d); refusing to signal it", PIDFile, pid)
			}
			if procx.Alive(pid) {
				return fmt.Errorf("sleeper: SIGTERM pid %d: %w", pid, err)
			}
		}
		if !procx.WaitExit(ctx, pid, termTimeout) {
			_ = procx.SignalGroup(pid, syscall.SIGKILL)
			if !procx.WaitExit(ctx, pid, termTimeout) {
				return fmt.Errorf("sleeper: pid %d did not exit after SIGKILL", pid)
			}
		}
	}
	if err := os.Remove(p.pidFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A sleeper killed outright leaves its control socket behind; nothing
	// answers on it, so it is only litter.
	if sock := p.ControlPath(); !Answers(sock) {
		if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Cleanup removes the control socket when backend.SocketPath placed it
// outside the machine directory, for "jm rm".
func (p Process) Cleanup() error {
	sock := p.ControlPath()
	if backend.InTree(p.Dir, sock) {
		return nil
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ErrAlreadyRunning is returned by LockPID when another sleeper holds the
// machine's pid file.
var ErrAlreadyRunning = errors.New("sleeper: another sleeper is running for this machine")

// LockPID takes an exclusive lock on the pid file and writes this process's
// pid into it. It is how a sleeper makes sure it is the only one; release
// removes the file (if it still names this process) and drops the lock.
func (p Process) LockPID() (release func(), err error) {
	f, err := os.OpenFile(p.pidFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("sleeper: locking %s: %w", p.pidFile(), err)
	}
	if err := f.Truncate(0); err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	return func() {
		if pid, err := readPID(p.pidFile()); err == nil && pid == os.Getpid() {
			_ = os.Remove(p.pidFile())
		}
		_ = f.Close()
	}, nil
}

// Waker keeps at most one "_wake" child alive and backs off after a child
// that did not wake the machine. It knows nothing about the machine: the
// caller decides when a child counts as failed.
type Waker struct {
	// Spawn launches a child and returns its pid.
	Spawn func() (int, error)
	// Alive reports whether a pid is still running (procx.Alive).
	Alive func(pid int) bool
	// Backoff is how long Ensure refuses to spawn after StartBackoff.
	Backoff time.Duration
	// Now is time.Now, replaced in tests.
	Now func() time.Time

	mu    sync.Mutex
	pid   int
	until time.Time
}

func (w *Waker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Ensure spawns a child unless one is alive or the backoff has not lapsed.
// It reports whether it spawned one.
func (w *Waker) Ensure() (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pid != 0 && w.Alive(w.pid) {
		return false, nil
	}
	if w.now().Before(w.until) {
		return false, nil
	}
	pid, err := w.Spawn()
	if err != nil {
		w.until = w.now().Add(w.Backoff)
		return false, err
	}
	w.pid = pid
	return true, nil
}

// Running is the pid of a live child, 0 when there is none.
func (w *Waker) Running() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pid != 0 && w.Alive(w.pid) {
		return w.pid
	}
	return 0
}

// Reap reports whether the child spawned last has exited since the previous
// call, and forgets it.
func (w *Waker) Reap() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pid == 0 || w.Alive(w.pid) {
		return false
	}
	w.pid = 0
	return true
}

// Forget drops the child spawned last without a backoff, alive or not. The
// sleeper calls it once the machine is seen running with no saved-state
// journal, or when a suspend starts: that child did its job. Without it, a
// child that exited after a successful wake would still be on record when the
// machine is suspended again before the next Reap, and read as a wake that
// left the machine suspended.
func (w *Waker) Forget() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pid = 0
}

// StartBackoff refuses new children for Backoff from now.
func (w *Waker) StartBackoff() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.until = w.now().Add(w.Backoff)
}

// BackingOff reports whether Ensure would refuse for the backoff.
func (w *Waker) BackingOff() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now().Before(w.until)
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("sleeper: bad pid file %s: %q", path, strings.TrimSpace(string(data)))
	}
	return pid, nil
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

// tail returns the trimmed tail of a log file, or a hint when it is empty.
func tail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "(no output in " + path + ")"
	}
	const max = 2048
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return strings.TrimSpace(string(data))
}
