package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// The resolver runs as a detached "jm _resolver <name>" (launched through
// procx, with a pid file and a log file) so it outlives the "jm start" that launched it, and is
// recognised by pid plus argv like the hypervisor, gvproxy and the port
// forwarder: pid files survive reboots and pids are recycled (ADR 0005).
//
// It also publishes the port it bound in AddrFile, because the guest's
// forwarder has to be told where to send queries and the port is chosen at
// runtime: the host resolver cannot use port 53, which needs root.

// Command is the hidden subcommand name.
const Command = "_resolver"

// Files kept in the machine directory.
const (
	PIDFile  = "resolver.pid"
	LogFile  = "resolver.log"
	AddrFile = "resolver.addr"
	// PortFile remembers the port the last run bound. AddrFile says where
	// a resolver is answering *now* and is removed when one is started, so
	// the launcher can tell a fresh address from a stale one; PortFile
	// outlives it, so a restarted resolver comes back on the same port and
	// the guest's configuration stays valid.
	PortFile = "resolver.port"
)

// Timeouts for Stop and for waiting on a freshly started resolver.
const (
	termTimeout  = 5 * time.Second
	startTimeout = 10 * time.Second
	pollInterval = 100 * time.Millisecond
)

// commandLineOf is commandLine, replaced in tests.
var commandLineOf = commandLine

// Process locates one machine's resolver.
type Process struct {
	Dir  string // machine directory
	Name string // machine name (appears in argv)
	Root string // state root (appears in argv)
}

func (p Process) pidFile() string { return filepath.Join(p.Dir, PIDFile) }

// LogPath is the resolver's log file.
func (p Process) LogPath() string { return filepath.Join(p.Dir, LogFile) }

// AddrPath is the file the running resolver publishes its address in.
func (p Process) AddrPath() string { return filepath.Join(p.Dir, AddrFile) }

// Args returns the argument vector (without argv[0]) the resolver is
// launched with; Alive matches a live process against it.
func (p Process) Args() []string {
	return []string{"--state-root", p.Root, Command, p.Name}
}

// Alive reports whether the pid in resolver.pid is a live resolver for this
// machine, and its pid.
func (p Process) Alive() (int, bool) {
	pid, err := readPID(p.pidFile())
	if err != nil {
		return 0, false
	}
	return pid, IsOurs(commandLineOf(pid), p)
}

// IsOurs matches argv against the substrings that identify this machine's
// resolver: "--state-root <root>" and "_resolver <name>". Substring matching
// keeps a state root containing spaces recognisable (ps prints argv joined
// by spaces).
func IsOurs(argv string, p Process) bool {
	argv = " " + argv + " "
	return strings.Contains(argv, " --state-root "+p.Root+" ") &&
		strings.Contains(argv, " "+Command+" "+p.Name+" ")
}

// Addr returns the address the running resolver published, "" when there is
// none.
func (p Process) Addr() string {
	data, err := os.ReadFile(p.AddrPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Port returns the port from the published address, 0 when there is none.
func (p Process) Port() int {
	_, port, err := net.SplitHostPort(p.Addr())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

// PortPath is the file remembering the last port used.
func (p Process) PortPath() string { return filepath.Join(p.Dir, PortFile) }

// LastPort is the port the previous run bound, 0 when there was none.
func (p Process) LastPort() int {
	data, err := os.ReadFile(p.PortPath())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// PublishAddr records the address the resolver bound, for the launcher and
// for "jm inspect", and remembers its port for the next run.
func (p Process) PublishAddr(addr string) error {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		if err := os.WriteFile(p.PortPath(), []byte(port+"\n"), 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(p.AddrPath(), []byte(addr+"\n"), 0o600)
}

// Start launches the resolver detached unless one is already alive, and
// waits until it publishes its address. exe is the jm binary.
func (p Process) Start(ctx context.Context, exe string) error {
	if _, ok := p.Alive(); ok && p.Addr() != "" {
		return nil
	}
	if err := p.Stop(ctx); err != nil {
		return err
	}
	// A stale address would be handed to the guest before the new resolver
	// has published its own.
	_ = os.Remove(p.AddrPath())
	// Detached through procx: the resolver must outlive this jm invocation
	// and must not become a zombie of a wrapper that execs podman.
	pid, err := procx.StartDetached(exe, p.Args(), nil, p.LogPath(), false)
	if err != nil {
		return fmt.Errorf("resolver: failed to start: %w", err)
	}
	if err := os.WriteFile(p.pidFile(), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return fmt.Errorf("resolver: writing %s: %w", p.pidFile(), err)
	}
	deadline := time.Now().Add(startTimeout)
	for {
		if p.Addr() != "" {
			return nil
		}
		if !procx.Alive(pid) {
			_ = os.Remove(p.pidFile())
			return fmt.Errorf("resolver: exited before publishing its address: %s", tail(p.LogPath()))
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_ = os.Remove(p.pidFile())
			return fmt.Errorf("resolver: timed out waiting for it to publish its address: %s", tail(p.LogPath()))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Stop terminates a live resolver (SIGTERM, wait, SIGKILL) and removes its
// pid file. A dead or absent one is just tidied away. The published address
// is kept: the next start reuses the port, so the guest's configuration
// stays valid across a restart. Signals go to the resolver's process group
// (procx.SignalGroup); Stop refuses to signal this process, and never
// signals its caller's process group.
func (p Process) Stop(ctx context.Context) error {
	if pid, ok := p.Alive(); ok {
		if err := procx.SignalGroup(pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, procx.ErrSelf) {
				return fmt.Errorf("resolver: %s names this process (pid %d); refusing to signal it", PIDFile, pid)
			}
			if procx.Alive(pid) {
				return fmt.Errorf("resolver: SIGTERM pid %d: %w", pid, err)
			}
		}
		if !procx.WaitExit(ctx, pid, termTimeout) {
			_ = procx.SignalGroup(pid, syscall.SIGKILL)
			if !procx.WaitExit(ctx, pid, termTimeout) {
				return fmt.Errorf("resolver: pid %d did not exit after SIGKILL", pid)
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
		return 0, fmt.Errorf("resolver: bad pid file %s: %q", path, strings.TrimSpace(string(data)))
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
