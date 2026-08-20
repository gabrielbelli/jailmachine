package qemu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// readPID parses a qemu pid file. It returns os.ErrNotExist (wrapped) when
// the file is absent.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("qemu: bad pid file %s: %q", path, strings.TrimSpace(string(data)))
	}
	return pid, nil
}

// processAlive is kill -0: true if the process exists (or exists but is not
// ours). Only used to wait for an exit after State has confirmed the pid is
// our QEMU; never to decide that a machine is running.
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

// commandLine returns the full argv of pid as reported by ps, or "" if there
// is no such process. ps is used because it works unprivileged on macOS,
// Linux and FreeBSD alike; -ww disables column truncation.
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

// isOurQEMU reports whether pid is a live qemu-system process started for
// the given pid file. A pid file survives host reboots and QEMU crashes, and
// the kernel recycles pids, so liveness alone is not enough (ADR 0005: a
// stale pid file is "broken", not "running"). Matching on the -pidfile
// argument also tells two machines' QEMUs apart.
func isOurQEMU(pid int, pidFile string) bool {
	argv := commandLine(pid)
	return strings.Contains(argv, Binary) && strings.Contains(argv, "-pidfile "+pidFile)
}

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
func kill(pid int) error      { return syscall.Kill(pid, syscall.SIGKILL) }
