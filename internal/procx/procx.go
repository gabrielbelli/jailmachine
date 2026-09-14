// Package procx launches and watches jm's detached helper processes: the
// hypervisor, the network provider, the podman socket forward and jm's own
// helpers.
//
// Every detached launch goes through a /bin/sh intermediate that starts the
// child in the background, prints its pid and exits. The child is then
// reparented to launchd (init), which reaps it when it exits. Without the
// intermediate the launching jm would be the parent, and a wrapper that
// execs podman or docker after a start never reaps its children, so a helper
// that exits later would stay a zombie that kill -0 still reports as alive.
package procx

import (
	"bytes"
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

// LogEnv carries the log path from StartDetached to the intermediate shell.
// The shell unsets it before it starts the child, so the child never sees it.
const LogEnv = "JM_LOG"

// launchScript backgrounds "$0" "$@" with stdin from /dev/null and stdout and
// stderr appended to the log, then prints the background pid. Both output
// descriptors are redirected, so the child never holds the pipe the caller
// reads the pid from.
const launchScript = `log=$JM_LOG; unset JM_LOG; "$0" "$@" </dev/null >>"$log" 2>&1 & echo $!`

// PollInterval is how often WaitExit checks a process.
const PollInterval = 50 * time.Millisecond

// ErrSelf is returned by SignalGroup when the target is this process.
var ErrSelf = errors.New("procx: refusing to signal this process")

// StartDetached runs bin via a /bin/sh intermediate that backgrounds it and
// exits, so the child is reparented to launchd and never becomes a zombie of
// a process that later execs podman or docker. The intermediate is a session
// leader (Setsid), so the child has no controlling terminal and shares a
// process group only with its own descendants.
//
// env is the child's environment; nil inherits this process's. The child's
// stdout and stderr go to logPath, which is created with mode 0600 and
// truncated first when truncate is set, otherwise appended to. The returned
// pid is the child's own pid, not the intermediate's.
func StartDetached(bin string, args []string, env []string, logPath string, truncate bool) (int, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return 0, fmt.Errorf("procx: %w", err)
	}
	// The launch runs in "/" (below), so a relative binary path must be
	// resolved here, against this process's working directory.
	if path, err = filepath.Abs(path); err != nil {
		return 0, fmt.Errorf("procx: %w", err)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if truncate {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	logf, err := os.OpenFile(logPath, flags, 0o600)
	if err != nil {
		return 0, fmt.Errorf("procx: opening %s: %w", logPath, err)
	}
	_ = logf.Close()

	if env == nil {
		env = os.Environ()
	}
	cmd := exec.Command("/bin/sh", append([]string{"-c", launchScript, path}, args...)...)
	cmd.Env = append(append([]string(nil), env...), LogEnv+"="+logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Long-lived helpers must not pin the caller's working directory (an
	// external volume could then not be ejected while the machine runs), as
	// QEMU's -daemonize used to guarantee with chdir("/"). Every path jm
	// hands a helper is absolute.
	cmd.Dir = "/"
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("procx: launching %s: %v: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("procx: launching %s: no pid from the launcher (%q)", path, strings.TrimSpace(string(out)))
	}
	return pid, nil
}

// Alive is kill -0 that also treats a zombie (ps stat Z) as dead. A process
// that exists but belongs to another user (EPERM) is alive.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !zombie(pid)
}

// WaitExit polls until pid is no longer Alive, the timeout lapses or ctx is
// cancelled. It returns true if the process has exited.
func WaitExit(ctx context.Context, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !Alive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return !Alive(pid)
		case <-time.After(PollInterval):
		}
	}
}

// SignalGroup sends sig to the process group pid belongs to, so children
// still in the group go with it, and to pid alone when the group cannot be
// found. It never signals this process (ErrSelf) and never the caller's own
// process group: a pid there is signalled on its own.
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("procx: bad pid %d", pid)
	}
	if pid == os.Getpid() {
		return ErrSelf
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid > 1 && pgid != syscall.Getpgrp() {
		if err := syscall.Kill(-pgid, sig); err == nil {
			return nil
		}
	}
	return syscall.Kill(pid, sig)
}
