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
// suspended (ADR 0009), so the forwarder never removes it and never binds it
// directly: ssh binds a name beside it (Paths.FwdSock), and once that answers
// it is renamed over podman.sock, so a client never finds the path missing.
// (ssh's own StreamLocalBindUnlink unlinks and then binds, which would leave
// a moment with no socket.) The inode is recorded in forward.ino, and a stop
// removes the socket only while it is still that inode. Nothing here decides
// who owns the path with a dial: a dial to a stand-in is a held connection,
// and a held connection wakes the machine.

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
	// A forward started before the beside name was used bound podman.sock
	// itself.
	bound := strings.Contains(argv, p.FwdSock+":") || strings.Contains(argv, p.Podman+":")
	return pid, strings.Contains(argv, "ssh") && bound
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
// socket it bound, and waits until the helper's socket answers beside the
// path before renaming it over the path.
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
	// Whatever is at the beside name is litter of an earlier helper; what is
	// at the path itself (a stand-in's socket) stays until the rename.
	if err := removeAll(p.FwdSock); err != nil {
		return fmt.Errorf("gvproxy: %w", err)
	}
	// Detached through procx: the forward must outlive this jm invocation.
	pid, err := procx.StartDetached(bin, sshx.ForwardArgs(machine.SSHHost, m.SSHPort, m.SSHUser, p.Key, p.FwdSock, machine.GuestPodmanSocket), nil, p.FwdLog, true)
	if err != nil {
		return fmt.Errorf("gvproxy: starting podman socket forward: %w", err)
	}
	if err := os.WriteFile(p.FwdPID, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return fmt.Errorf("gvproxy: writing %s: %w", p.FwdPID, err)
	}
	fail := func(err error) error {
		if procx.Alive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			procx.WaitExit(context.Background(), pid, stopTimeout)
		}
		_ = removeAll(p.FwdSock, p.FwdPID, p.FwdIno) // the helper's own files only
		return fmt.Errorf("gvproxy: podman socket forward: %w: %s", err, tailOf(p.FwdLog))
	}
	ino, err := waitNewSocket(ctx, pid, p.FwdSock, 0, false)
	if err != nil {
		return fail(err)
	}
	if err := os.Rename(p.FwdSock, p.Podman); err != nil {
		return fail(err)
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
// before inodes were recorded, when that helper was just stopped), or when
// it is not a socket at all: a socket that replaced it belongs to someone
// else.
func stopForward(ctx context.Context, p Paths, keepSocket bool) error {
	pid, killed := forwardAlive(p)
	if killed {
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
		_, isSocket := socketInode(p.Podman)
		if exists && ((recorded && cur == want) || (!recorded && killed) || !isSocket) {
			errs = append(errs, removeAll(p.Podman))
		}
	}
	errs = append(errs, removeAll(p.FwdSock, p.FwdPID, p.FwdIno))
	err := errors.Join(errs...)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
