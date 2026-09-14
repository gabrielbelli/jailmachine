//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Defaults for the knobs in doc.go. The disk is small on purpose: jm init
// writes far more real disk than the image holds (docs/LIMITATIONS.md), and
// the suite must fit on a laptop volume with a few tens of GiB free.
const (
	defaultE2EDiskGiB = 12
	defaultE2EImage   = "official:15.1-RELEASE"
)

// Command limits. No command may hang the suite into go test's -timeout:
// that panics the test binary without running any t.Cleanup, and the
// machine's detached processes would outlive it.
const (
	// cmdTimeout bounds one command.
	cmdTimeout = 10 * time.Minute
	// provisionTimeout bounds jm init and the first jm start, which fetch
	// the image and, for an official image, provision the guest.
	provisionTimeout = 40 * time.Minute
	// cleanupReserve is kept free before go test's deadline for cleanups.
	cleanupReserve = 5 * time.Minute
	// cleanupTimeout bounds the jm rm --force a cleanup runs.
	cleanupTimeout = 4 * time.Minute
	// waitDelay is how long output pipes may stay open after a command is
	// killed: a child such as ssh can hold them.
	waitDelay = 10 * time.Second
)

// e2eConfig is the environment an end-to-end test runs with.
type e2eConfig struct {
	bin     string // absolute path of the built ./jm
	image   string // JM_E2E_IMAGE
	diskGiB int    // JM_E2E_DISK
	slow    bool   // JM_E2E_SLOW=1
}

// requireE2E skips t unless JM_E2E=1 and returns the suite's configuration.
func requireE2E(t *testing.T) e2eConfig {
	t.Helper()
	if os.Getenv("JM_E2E") != "1" {
		t.Skip("set JM_E2E=1 to run the end-to-end tests")
	}
	bin, err := filepath.Abs("../jm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("%s not built (run make build): %v", bin, err)
	}
	cfg := e2eConfig{bin: bin, image: defaultE2EImage, diskGiB: defaultE2EDiskGiB, slow: os.Getenv("JM_E2E_SLOW") == "1"}
	if v := os.Getenv("JM_E2E_IMAGE"); v != "" {
		cfg.image = v
	}
	if v := os.Getenv("JM_E2E_DISK"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("JM_E2E_DISK=%q: want a positive number of GiB", v)
		}
		cfg.diskGiB = n
	}
	return cfg
}

// harness runs jm against one machine in a private state root.
type harness struct {
	bin, root, name string
}

// result is one finished command.
type result struct {
	stdout, stderr string
	err            error
	took           time.Duration
}

func (r result) combined() string { return r.stdout + r.stderr }

// exitCode is the command's exit status, or -1 when it did not exit normally.
func (r result) exitCode() int {
	var exit *exec.ExitError
	if errors.As(r.err, &exit) {
		return exit.ExitCode()
	}
	if r.err == nil {
		return 0
	}
	return -1
}

// limit is d, shortened so that cleanupReserve is left before go test's
// deadline.
func limit(t *testing.T, d time.Duration) time.Duration {
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) - cleanupReserve; left < d {
			d = max(left, 30*time.Second)
		}
	}
	return d
}

// runTimed runs cmd, killing it after d. It reports whether it was killed.
func runTimed(cmd *exec.Cmd, d time.Duration) (bool, error) {
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return false, err
	}
	var fired atomic.Bool
	timer := time.AfterFunc(d, func() {
		fired.Store(true)
		_ = cmd.Process.Kill()
	})
	err := cmd.Wait()
	timer.Stop()
	return fired.Load(), err
}

// runCmd runs cmd under cmdTimeout, capturing and logging its output.
func runCmd(t *testing.T, cmd *exec.Cmd) result {
	t.Helper()
	return runCmdFor(t, cmd, limit(t, cmdTimeout))
}

// runCmdFor runs cmd, killed after d, capturing and logging its output.
func runCmdFor(t *testing.T, cmd *exec.Cmd, d time.Duration) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	began := time.Now()
	timedOut, err := runTimed(cmd, d)
	r := result{stdout: stdout.String(), stderr: stderr.String(), err: err, took: time.Since(began)}
	if timedOut {
		r.err = fmt.Errorf("killed after %s: %w", d, err)
	}
	t.Logf("$ %s (%.1f s, exit %d)\n%s%s", strings.Join(cmd.Args, " "), r.took.Seconds(), r.exitCode(), r.stdout, r.stderr)
	return r
}

// output runs cmd, killed after d, and returns its standard output without
// logging it: for polls.
func output(cmd *exec.Cmd, d time.Duration) (string, error) {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_, err := runTimed(cmd, d)
	return stdout.String(), err
}

// lockedBuffer is a bytes.Buffer safe to read while a command writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// background is a command that runs while the test does something else.
type background struct {
	cmd  *exec.Cmd
	out  lockedBuffer
	done chan struct{}
	err  error // valid once done is closed
}

// startBackground starts cmd with its output collected. It is killed after
// d, and by t's cleanup if it still runs then.
func startBackground(t *testing.T, cmd *exec.Cmd, d time.Duration) *background {
	t.Helper()
	b := &background{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &b.out, &b.out
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", strings.Join(cmd.Args, " "), err)
	}
	timer := time.AfterFunc(d, func() { _ = cmd.Process.Kill() })
	go func() {
		b.err = cmd.Wait()
		timer.Stop()
		close(b.done)
	}()
	t.Cleanup(b.kill)
	return b
}

// exited reports whether the command has finished.
func (b *background) exited() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// kill SIGKILLs the command unless it has finished, and waits for it.
func (b *background) kill() {
	if !b.exited() {
		_ = b.cmd.Process.Kill()
	}
	<-b.done
}

// run runs jm with args under cmdTimeout and returns what happened.
func (h *harness) run(t *testing.T, args ...string) result {
	t.Helper()
	return runCmd(t, h.command(args...))
}

// command is an unstarted jm command.
func (h *harness) command(args ...string) *exec.Cmd {
	return exec.Command(h.bin, append([]string{"--state-root", h.root}, args...)...)
}

// wrapper is an unstarted client wrapper ("podman" or "docker" and its
// arguments, as jpodman and jdocker run them) aimed at the machine.
func (h *harness) wrapper(args ...string) *exec.Cmd {
	c := h.command(args...)
	c.Env = append(os.Environ(), "JM_MACHINE="+h.name)
	return c
}

// jm runs jm under cmdTimeout and fails t when it exits non-zero; it
// returns stdout and stderr together.
func (h *harness) jm(t *testing.T, args ...string) string {
	t.Helper()
	return h.jmFor(t, cmdTimeout, args...)
}

// jmFor is jm with its own time limit.
func (h *harness) jmFor(t *testing.T, d time.Duration, args ...string) string {
	t.Helper()
	r := runCmdFor(t, h.command(args...), limit(t, d))
	if r.err != nil {
		t.Fatalf("jm %s: %v", strings.Join(args, " "), r.err)
	}
	return r.combined()
}

// guest runs a shell command line in the guest over jm ssh and returns its
// standard output. ssh joins its arguments into one string for the remote
// shell, so script is passed whole and quoted by the caller.
func (h *harness) guest(t *testing.T, script string) result {
	t.Helper()
	return h.run(t, "ssh", h.name, "--", script)
}

// mustGuest is guest failing t on a non-zero exit; it returns trimmed stdout.
func (h *harness) mustGuest(t *testing.T, script string) string {
	t.Helper()
	r := h.guest(t, script)
	if r.err != nil {
		t.Fatalf("guest %q: %v", script, r.err)
	}
	return strings.TrimSpace(r.stdout)
}

// dir is the machine's directory.
func (h *harness) dir() string { return filepath.Join(h.root, "machines", h.name) }

// machineInfo is the part of "jm --json inspect" the tests read.
type machineInfo struct {
	Name                    string `json:"name"`
	State                   string `json:"state"`
	APISocket               string `json:"api_socket"`
	SleeperState            string `json:"sleeper_state"`
	SuspendImage            string `json:"suspend_image"`
	SuspendImageBytes       int64  `json:"suspend_image_bytes"`
	IdleSuspendMin          int    `json:"idle_suspend_min"`
	IdleSuspendAfterSeconds int64  `json:"idle_suspend_after_seconds"`
	Shares                  []struct {
		HostPath  string `json:"host_path"`
		GuestPath string `json:"guest_path"`
		ReadOnly  bool   `json:"read_only"`
	} `json:"shares"`
}

// inspect is "jm --json inspect <name>".
func (h *harness) inspect(t *testing.T) machineInfo {
	t.Helper()
	r := h.run(t, "--json", "inspect", h.name)
	if r.err != nil {
		t.Fatalf("jm --json inspect: %v", r.err)
	}
	var i machineInfo
	if err := json.Unmarshal([]byte(r.stdout), &i); err != nil {
		t.Fatalf("decoding jm --json inspect: %v", err)
	}
	return i
}

// listState is the machine's state in "jm --json list", or "" when absent.
func (h *harness) listState(t *testing.T) string {
	t.Helper()
	r := h.run(t, "--json", "list")
	if r.err != nil {
		t.Fatalf("jm --json list: %v", r.err)
	}
	var infos []machineInfo
	if err := json.Unmarshal([]byte(r.stdout), &infos); err != nil {
		t.Fatalf("decoding jm --json list: %v", err)
	}
	for _, i := range infos {
		if i.Name == h.name {
			return i.State
		}
	}
	return ""
}

// wantState fails t unless both jm --json list and jm --json inspect report
// the machine in state.
func (h *harness) wantState(t *testing.T, state string) {
	t.Helper()
	if got := h.listState(t); got != state {
		t.Fatalf("jm --json list: %s is %q, want %q", h.name, got, state)
	}
	if got := h.inspect(t).State; got != state {
		t.Fatalf("jm --json inspect: %s is %q, want %q", h.name, got, state)
	}
}

// podman runs the host podman against a connection and fails t on error.
func podman(t *testing.T, connection string, args ...string) string {
	t.Helper()
	r := runCmd(t, exec.Command("podman", append([]string{"--connection", connection}, args...)...))
	if r.err != nil {
		t.Fatalf("podman --connection %s %s: %v", connection, strings.Join(args, " "), r.err)
	}
	return r.combined()
}

// httpdArgs serve "ok" on port 80. busybox httpd rather than nginx: nginx's
// workers need Linux AIO (io_setup), which the Linuxulator does not
// implement, so nginx accepts connections but never answers them.
var httpdArgs = []string{"--os=linux", "docker.io/busybox", "sh", "-c",
	"mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 80 -h /www"}

// curlOK reports whether url answers "ok".
func curlOK(url string) bool {
	out, err := exec.Command("curl", "-fsS", "--max-time", "3", url).Output()
	return err == nil && strings.TrimSpace(string(out)) == "ok"
}

// waitFor polls cond every interval until it is true or timeout passes.
func waitFor(timeout, interval time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// inodeOf is path's inode number.
func inodeOf(path string) (uint64, bool) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return sys.Ino, true
}

// pidFromFile reads a pid file; ok is false when it is missing or bad.
func pidFromFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid, err == nil && pid > 0
}

// processAlive reports whether pid runs and is not a zombie.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	return err == nil && stat != "" && !strings.HasPrefix(stat, "Z")
}

// pidFileAlive is the live process a pid file names, if any.
func pidFileAlive(path string) (int, bool) {
	pid, ok := pidFromFile(path)
	return pid, ok && processAlive(pid)
}
