package gvproxy

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

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/procx"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// The podman.sock forwarder is a detached system ssh running
// "-N -L <podman.sock>:/var/run/podman/podman.sock" through gvproxy's
// -ssh-port. Like gvproxy it has a pid file and a log, and is recognised
// by pid plus argv (the host socket path is unique per machine).
//
// The socket path can be held by someone else while the machine is
// suspended (ADR 0009), so the forwarder never removes it before it starts:
// ssh's StreamLocalBindUnlink replaces the file, and startForward waits for
// a socket with a different inode that answers. The inode the helper bound
// is recorded in forward.ino, and a stop removes the socket only while it is
// still that inode.

// Forward readiness. Variables so tests can shorten them.
var (
	forwardTimeout      = startTimeout
	forwardPollInterval = 20 * time.Millisecond
)

// dialTimeout bounds the dial that tells a served socket from a stale one.
const dialTimeout = 100 * time.Millisecond

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

// socketInode returns the inode of the unix socket at path, false when there
// is no socket there.
func socketInode(path string) (uint64, bool) {
	st, err := os.Lstat(path)
	if err != nil || st.Mode().Type() != os.ModeSocket {
		return 0, false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(s.Ino), true
}

// fileInode returns the inode of whatever is at path.
func fileInode(path string) (uint64, bool) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(s.Ino), true
}

// socketAnswers reports whether a unix dial to path succeeds.
func socketAnswers(path string) bool {
	c, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// staleSocket reports whether something is at path and nothing answers
// there.
func staleSocket(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	return !socketAnswers(path)
}

// readInode parses forward.ino.
func readInode(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	ino, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return ino, err == nil
}

// startForward launches the forwarder unless one is already serving the
// socket it bound, and waits until the path is a new socket that answers.
func startForward(ctx context.Context, m *machine.Machine, p Paths) error {
	if _, ok := forwardAlive(p); ok {
		want, recorded := readInode(p.FwdIno)
		if cur, ok := socketInode(p.Podman); ok && recorded && cur == want && socketAnswers(p.Podman) {
			return nil
		}
	}
	_ = stopForward(ctx, p, false)
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("gvproxy: ssh binary not found for the podman socket forward: %w", err)
	}
	// Whatever is at the path now (a stand-in's socket) must not satisfy
	// the wait below, and must not be removed if the helper fails.
	before, existed := fileInode(p.Podman)
	// Detached through procx: the forward must outlive this jm invocation.
	pid, err := procx.StartDetached(bin, sshx.ForwardArgs(machine.SSHHost, m.SSHPort, m.SSHUser, p.Key, p.Podman, machine.GuestPodmanSocket), nil, p.FwdLog, true)
	if err != nil {
		return fmt.Errorf("gvproxy: starting podman socket forward: %w", err)
	}
	if err := os.WriteFile(p.FwdPID, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return fmt.Errorf("gvproxy: writing %s: %w", p.FwdPID, err)
	}
	ino, err := waitNewSocket(ctx, pid, p.Podman, before, existed)
	if err != nil {
		if procx.Alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			procx.WaitExit(context.Background(), pid, stopTimeout)
		}
		if cur, ok := fileInode(p.Podman); ok && (!existed || cur != before) {
			_ = removeAll(p.Podman) // the helper's own leftover only
		}
		_ = removeAll(p.FwdPID, p.FwdIno)
		return fmt.Errorf("gvproxy: podman socket forward: %w: %s", err, tailOf(p.FwdLog))
	}
	if err := os.WriteFile(p.FwdIno, []byte(strconv.FormatUint(ino, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("gvproxy: writing %s: %w", p.FwdIno, err)
	}
	return nil
}

// waitNewSocket polls until path is a socket that was not there before (a
// different inode from before, when existed) and a dial to it succeeds. It
// fails when pid exits, the timeout lapses or ctx is cancelled.
func waitNewSocket(ctx context.Context, pid int, path string, before uint64, existed bool) (uint64, error) {
	deadline := time.Now().Add(forwardTimeout)
	for {
		if cur, ok := socketInode(path); ok && (!existed || cur != before) && socketAnswers(path) {
			return cur, nil
		}
		if !procx.Alive(pid) {
			return 0, fmt.Errorf("exited before serving %s", filepath.Base(path))
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("timed out waiting for a new %s", filepath.Base(path))
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(forwardPollInterval):
		}
	}
}

// stopForward terminates a live forwarder and removes its pid and inode
// files. Unless keepSocket is set, podman.sock is removed too, but only
// while it is still the socket the helper bound (or, for a helper started
// before inodes were recorded, while nothing answers on it): a socket that
// replaced it belongs to someone else.
func stopForward(ctx context.Context, p Paths, keepSocket bool) error {
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
	var errs []error
	if !keepSocket {
		want, recorded := readInode(p.FwdIno)
		cur, exists := fileInode(p.Podman)
		if exists && ((recorded && cur == want) || (!recorded && staleSocket(p.Podman))) {
			errs = append(errs, removeAll(p.Podman))
		}
	}
	errs = append(errs, removeAll(p.FwdPID, p.FwdIno))
	err := errors.Join(errs...)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
