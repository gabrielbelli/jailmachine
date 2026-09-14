package procx

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func parentOf(t *testing.T, pid int) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps ppid of %d: %v", pid, err)
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ps ppid of %d: %q", pid, out)
	}
	return ppid
}

// A detached child outlives the intermediate that launched it and belongs to
// launchd, not to this process, so nothing here has to reap it.
func TestStartDetachedReparents(t *testing.T) {
	log := filepath.Join(t.TempDir(), "child.log")
	pid, err := StartDetached("sleep", []string{"30"}, nil, log, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		WaitExit(context.Background(), pid, 5*time.Second)
	})
	if !Alive(pid) {
		t.Fatal("detached child is not alive")
	}
	if pid == os.Getpid() {
		t.Fatal("launcher returned this process's pid")
	}
	if ppid := parentOf(t, pid); ppid != 1 {
		t.Fatalf("parent of the detached child = %d, want 1 (launchd)", ppid)
	}
	if out, _ := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output(); !strings.Contains(string(out), "sleep") {
		t.Fatalf("pid %d is %q, not the child itself", pid, out)
	}
	if err := SignalGroup(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if !WaitExit(context.Background(), pid, 5*time.Second) {
		t.Fatal("child did not exit after SIGTERM")
	}
}

// kill -0 succeeds on a zombie; Alive must not.
func TestAliveFalseForZombie(t *testing.T) {
	// The shell backgrounds "sleep 0", prints its pid and execs a sleep that
	// never reaps it, so the first child stays a zombie.
	cmd := exec.Command("/bin/sh", "-c", `sleep 0 & echo $!; exec sleep 30`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("pid line %q", line)
	}
	if !WaitExit(context.Background(), pid, 5*time.Second) {
		t.Fatal("zombie still reported alive")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("kill -0 on the zombie failed (%v): the test did not make a zombie", err)
	}
	if Alive(pid) {
		t.Fatal("Alive is true for a zombie")
	}
	if !Alive(cmd.Process.Pid) {
		t.Fatal("the live parent is reported dead")
	}
	if Alive(0) || Alive(-1) || Alive(2147483646) {
		t.Fatal("impossible pids reported alive")
	}
}

func runToExit(t *testing.T, log string, truncate bool, env []string, script string) {
	t.Helper()
	pid, err := StartDetached("/bin/sh", []string{"-c", script}, env, log, truncate)
	if err != nil {
		t.Fatal(err)
	}
	if !WaitExit(context.Background(), pid, 5*time.Second) {
		t.Fatalf("pid %d did not exit", pid)
	}
}

func TestStartDetachedLogTruncateAndAppend(t *testing.T) {
	log := filepath.Join(t.TempDir(), "helper.log")
	if err := os.WriteFile(log, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	runToExit(t, log, true, nil, "echo one; echo err >&2")
	if got := read(); got != "one\nerr\n" {
		t.Fatalf("after truncate: %q", got)
	}
	runToExit(t, log, false, nil, "echo two")
	if got := read(); got != "one\nerr\ntwo\n" {
		t.Fatalf("after append: %q", got)
	}
	// The environment is the one given, and the launcher's own variable
	// does not reach the child.
	runToExit(t, log, true, []string{"PATH=/usr/bin:/bin", "JM_TEST_VALUE=bar"}, `echo "$JM_TEST_VALUE ${JM_LOG-unset}"`)
	if got := read(); got != "bar unset\n" {
		t.Fatalf("after truncate with env: %q", got)
	}
	st, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v, want 0600", st.Mode().Perm())
	}

	fresh := filepath.Join(t.TempDir(), "new.log")
	runToExit(t, fresh, false, nil, "echo hi")
	if st, err := os.Stat(fresh); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("new log: %v, %v", st, err)
	}
}

func TestStartDetachedMissingBinary(t *testing.T) {
	if _, err := StartDetached(filepath.Join(t.TempDir(), "nope"), nil, nil, filepath.Join(t.TempDir(), "x.log"), true); err == nil {
		t.Fatal("a missing binary must fail at launch")
	}
}

// SignalGroup never signals this process, and a pid in the caller's own
// process group is signalled alone, never the group.
func TestSignalGroupNeverSignalsSelf(t *testing.T) {
	if err := SignalGroup(os.Getpid(), syscall.SIGTERM); err != ErrSelf {
		t.Fatalf("SignalGroup(self) = %v, want ErrSelf", err)
	}
	cmd := exec.Command("sleep", "30") // same process group as the test
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if err := SignalGroup(cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child in our group was not signalled")
	}
	// Reaching this line means the test process itself was not signalled.
}

// A detached child runs in "/", so it never pins the caller's working
// directory, as QEMU's -daemonize used to guarantee.
func TestStartDetachedRunsInRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	log := filepath.Join(t.TempDir(), "child.log")
	pid, err := StartDetached("/bin/sh", []string{"-c", "pwd"}, nil, log, true)
	if err != nil {
		t.Fatal(err)
	}
	if !WaitExit(context.Background(), pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("child did not exit")
	}
	out, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "/" {
		t.Fatalf("child working directory = %q, want /", got)
	}
}
