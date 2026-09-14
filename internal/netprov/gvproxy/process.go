package gvproxy

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
