package gvproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// readPID parses a pid file; os.ErrNotExist (wrapped) when absent.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("gvproxy: bad pid file %s: %q", path, strings.TrimSpace(string(data)))
	}
	return pid, nil
}

// processAlive is kill -0. Only used to wait for an exit after State has
// confirmed the pid is ours.
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

// isOurs reports whether pid is a live gvproxy started for the given API
// socket: pids are recycled and pid files survive reboots (ADR 0005), and
// the api.sock path tells machines apart.
func isOurs(pid int, apiSock string) bool {
	argv := commandLine(pid)
	return strings.Contains(argv, Binary) && strings.Contains(argv, "unix://"+apiSock)
}

// waitExit polls until the process is gone or the timeout/ctx lapses.
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
