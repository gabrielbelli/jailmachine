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

	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// The forwarder runs as a detached "jm _forwarder <name>" (launched through
// procx, with a pid file and a log file) so it outlives the "jm start" that
// launched it, and is recognised by pid plus argv like the hypervisor and
// gvproxy: pid files survive reboots and pids are recycled (ADR 0005).

// Command is the hidden subcommand name.
const Command = "_forwarder"

// termTimeout bounds each wait in Stop.
const termTimeout = 5 * time.Second

// commandLineOf is commandLine, replaced in tests.
var commandLineOf = commandLine

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
	return pid, isOurs(commandLineOf(pid), p)
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
	// Detached through procx: the forwarder must outlive this jm invocation
	// and must not become a zombie of a wrapper that execs podman.
	pid, err := procx.StartDetached(exe, p.Args(), nil, p.LogPath(), false)
	if err != nil {
		return fmt.Errorf("forwarder: failed to start: %w", err)
	}
	if err := os.WriteFile(p.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return fmt.Errorf("forwarder: writing %s: %w", p.pidFile(), err)
	}
	return nil
}

// Stop terminates a live forwarder (SIGTERM, wait, SIGKILL) and removes the
// pid file. A dead or absent one is just tidied away. Signals go to the
// forwarder's process group, so children still in the group go with it
// (procx.SignalGroup). Stop refuses to signal this process, and never
// signals its caller's process group.
func (p Process) Stop(ctx context.Context) error {
	if pid, ok := p.Alive(); ok {
		if err := procx.SignalGroup(pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, procx.ErrSelf) {
				return fmt.Errorf("forwarder: %s names this process (pid %d); refusing to signal it", PIDFile, pid)
			}
			if procx.Alive(pid) {
				return fmt.Errorf("forwarder: SIGTERM pid %d: %w", pid, err)
			}
		}
		if !procx.WaitExit(ctx, pid, termTimeout) {
			_ = procx.SignalGroup(pid, syscall.SIGKILL)
			if !procx.WaitExit(ctx, pid, termTimeout) {
				return fmt.Errorf("forwarder: pid %d did not exit after SIGKILL", pid)
			}
		}
	}
	if err := os.Remove(p.pidFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
