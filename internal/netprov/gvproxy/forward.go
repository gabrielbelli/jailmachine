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

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/procx"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// The podman.sock forwarder is a detached system ssh running
// "-N -L <podman.sock>:/var/run/podman/podman.sock" through gvproxy's
// -ssh-port. Like gvproxy it has a pid file and a log, and is recognised
// by pid plus argv (the host socket path is unique per machine).

// forwardAlive reports whether the forwarder recorded in p.FwdPID is a live
// ssh serving p.Podman.
func forwardAlive(p Paths) (int, bool) {
	pid, err := readPID(p.FwdPID)
	if err != nil {
		return 0, false
	}
	argv := commandLine(pid)
	return pid, strings.Contains(argv, "ssh") && strings.Contains(argv, p.Podman+":")
}

// startForward launches the forwarder unless one is already serving the
// socket, and waits for the host socket to appear.
func startForward(ctx context.Context, m *machine.Machine, p Paths) error {
	if _, ok := forwardAlive(p); ok {
		if _, err := os.Stat(p.Podman); err == nil {
			return nil
		}
	}
	_ = stopForward(ctx, p)
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("gvproxy: ssh binary not found for the podman socket forward: %w", err)
	}
	_ = os.Remove(p.Podman)
	// Detached through procx: the forward must outlive this jm invocation.
	pid, err := procx.StartDetached(bin, sshx.ForwardArgs(machine.SSHHost, m.SSHPort, m.SSHUser, p.Key, p.Podman, machine.GuestPodmanSocket), nil, p.FwdLog, true)
	if err != nil {
		return fmt.Errorf("gvproxy: starting podman socket forward: %w", err)
	}
	if err := os.WriteFile(p.FwdPID, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return fmt.Errorf("gvproxy: writing %s: %w", p.FwdPID, err)
	}
	if err := waitSockets(ctx, pid, p.Podman); err != nil {
		if procx.Alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			procx.WaitExit(context.Background(), pid, stopTimeout)
		}
		_ = removeAll(p.FwdPID, p.Podman)
		return fmt.Errorf("gvproxy: podman socket forward: %w: %s", err, tailOf(p.FwdLog))
	}
	return nil
}

// stopForward terminates a live forwarder and removes its pid file and
// socket; a dead or absent one is just tidied away.
func stopForward(ctx context.Context, p Paths) error {
	if pid, ok := forwardAlive(p); ok {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && procx.Alive(pid) {
			return fmt.Errorf("gvproxy: SIGTERM forward pid %d: %w", pid, err)
		}
		if !procx.WaitExit(ctx, pid, stopTimeout) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if !procx.WaitExit(ctx, pid, stopTimeout) {
				return fmt.Errorf("gvproxy: forward pid %d did not exit after SIGKILL", pid)
			}
		}
	}
	err := removeAll(p.FwdPID, p.Podman)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
