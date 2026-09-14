package sleeper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProcessIdentity(t *testing.T) {
	dir := t.TempDir()
	p := Process{Dir: dir, Name: "dev", Root: "/state root"}
	if got := p.Args(); !slices.Equal(got, []string{"--state-root", "/state root", "_sleeper", "dev"}) {
		t.Fatalf("Args = %q", got)
	}
	if got := p.WakeArgs(); !slices.Equal(got, []string{"--state-root", "/state root", "_wake", "dev"}) {
		t.Fatalf("WakeArgs = %q", got)
	}
	if !IsOurs("/usr/local/bin/jm --state-root /state root _sleeper dev", p) {
		t.Error("own argv not recognised")
	}
	for _, argv := range []string{
		"",
		"/usr/local/bin/jm --state-root /other _sleeper dev",
		"/usr/local/bin/jm --state-root /state root _sleeper other",
		"/usr/local/bin/jm --state-root /state root _wake dev",
		"/usr/local/bin/jm --state-root /state root _forwarder dev",
	} {
		if IsOurs(argv, p) {
			t.Errorf("argv wrongly recognised as ours: %q", argv)
		}
	}
	if p.LogPath() != filepath.Join(dir, LogFile) || p.StatusPath() != filepath.Join(dir, "sleeper.json") ||
		p.WakeLogPath() != filepath.Join(dir, "wake.log") {
		t.Errorf("paths = %q, %q, %q", p.LogPath(), p.StatusPath(), p.WakeLogPath())
	}
	if !strings.HasSuffix(p.ControlPath(), ".sock") {
		t.Errorf("control path = %q", p.ControlPath())
	}
}

func TestStopNeverSignalsSelf(t *testing.T) {
	dir := t.TempDir()
	p := Process{Dir: dir, Name: "dev", Root: "/state root"}
	child := exec.Command("sleep", "30") // shares the test's process group
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-exited })
	saved := commandLineOf
	t.Cleanup(func() { commandLineOf = saved })
	commandLineOf = func(pid int) string {
		if pid == os.Getpid() || pid == child.Process.Pid {
			return "/usr/local/bin/jm --state-root /state root _sleeper dev"
		}
		return ""
	}
	pidFile := filepath.Join(dir, PIDFile)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Stop with our own pid = %v, want a refusal", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("sleeper in our process group was not stopped")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("pid file not removed")
	}
	// A stale pid file is tidied away.
	if err := os.WriteFile(pidFile, []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("stale pid file not removed")
	}
}

func TestStartStripsJMEnv(t *testing.T) {
	env := ChildEnv([]string{"PATH=/bin", "JM_HOME=/x", "JM_MTU=1500", "HOME=/h", "XJM_KEEP=1", "JM_GVPROXY=/opt/gvproxy", "JM_GVPROXYX=1"}, "JM_GVPROXY")
	if !slices.Equal(env, []string{"PATH=/bin", "HOME=/h", "XJM_KEEP=1", "JM_GVPROXY=/opt/gvproxy"}) {
		t.Errorf("ChildEnv = %q", env)
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "fake-jm")
	script := "#!/bin/sh\necho \"argv: $*\"\nenv | grep -E '^(JM_|SLEEPER_TEST_)' | sort\nexit 1\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JM_PUBLISH_ADDR", "127.0.0.1")
	t.Setenv("JM_AUTOSTART", "0")
	t.Setenv("SLEEPER_TEST_KEEP", "1")
	t.Setenv("JM_GVPROXY", "/opt/gvproxy")
	p := Process{Dir: dir, Name: "dev", Root: "/state root", KeepEnv: []string{"JM_GVPROXY"}}
	err := p.Start(context.Background(), exe)
	if err == nil || !strings.Contains(err.Error(), "exited before answering") {
		t.Fatalf("Start = %v", err)
	}
	log, _ := os.ReadFile(p.LogPath())
	if !strings.Contains(string(log), "JM_GVPROXY=/opt/gvproxy") {
		t.Errorf("the sleeper lost the program locator:\n%s", log)
	}
	if strings.Contains(strings.ReplaceAll(string(log), "JM_GVPROXY=", ""), "JM_") {
		t.Errorf("the sleeper inherited JM_* variables:\n%s", log)
	}
	if !strings.Contains(string(log), "SLEEPER_TEST_KEEP=1") || !strings.Contains(string(log), "argv: --state-root /state root _sleeper dev") {
		t.Errorf("log = %s", log)
	}
}

func TestLockPIDSingleInstance(t *testing.T) {
	p := Process{Dir: t.TempDir(), Name: "dev", Root: "/r"}
	release, err := p.LockPID()
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := readPID(p.pidFile()); err != nil || pid != os.Getpid() {
		t.Errorf("pid file = %d, %v", pid, err)
	}
	if _, err := p.LockPID(); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second LockPID = %v", err)
	}
	release()
	if _, err := os.Stat(p.pidFile()); !os.IsNotExist(err) {
		t.Error("release left the pid file")
	}
	release, err = p.LockPID()
	if err != nil {
		t.Fatalf("LockPID after release = %v", err)
	}
	release()
}

func TestOneWakerWithBackoff(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	alive := map[int]bool{}
	spawned := 0
	w := &Waker{
		Spawn: func() (int, error) {
			spawned++
			pid := 100 + spawned
			alive[pid] = true
			return pid, nil
		},
		Alive:   func(pid int) bool { return alive[pid] },
		Backoff: 10 * time.Second,
		Now:     func() time.Time { return now },
	}
	if ok, err := w.Ensure(); !ok || err != nil {
		t.Fatalf("first Ensure = %v, %v", ok, err)
	}
	if ok, _ := w.Ensure(); ok || spawned != 1 || w.Running() != 101 {
		t.Fatalf("a second child was spawned while the first lives (%d)", spawned)
	}
	if w.Reap() {
		t.Error("Reap reported a live child as exited")
	}
	alive[101] = false
	if !w.Reap() || w.Reap() {
		t.Error("Reap must report an exited child exactly once")
	}
	w.StartBackoff()
	if ok, _ := w.Ensure(); ok || !w.BackingOff() {
		t.Error("spawned during the backoff")
	}
	now = now.Add(10 * time.Second)
	if ok, _ := w.Ensure(); !ok || spawned != 2 {
		t.Errorf("no spawn after the backoff (%d)", spawned)
	}
	// A spawn that fails backs off too.
	alive[102] = false
	w.Reap()
	w.Spawn = func() (int, error) { return 0, errors.New("no jm") }
	if ok, err := w.Ensure(); ok || err == nil || !w.BackingOff() {
		t.Errorf("failed spawn = %v, %v, backing off %v", ok, err, w.BackingOff())
	}
}

func TestWakerForgetDropsTheChildWithoutABackoff(t *testing.T) {
	alive := map[int]bool{}
	spawned := 0
	w := &Waker{
		Spawn:   func() (int, error) { spawned++; pid := 200 + spawned; alive[pid] = true; return pid, nil },
		Alive:   func(pid int) bool { return alive[pid] },
		Backoff: 10 * time.Second,
	}
	if ok, err := w.Ensure(); !ok || err != nil {
		t.Fatalf("Ensure = %v, %v", ok, err)
	}
	// The child did its job and exits; the sleeper forgets it before anything
	// reaps it, so the exit is never read as a failed wake.
	w.Forget()
	alive[201] = false
	if w.Reap() {
		t.Error("Reap reported a forgotten child")
	}
	if w.BackingOff() {
		t.Error("Forget started a backoff")
	}
	// A live child can be forgotten too; the next Ensure spawns at once.
	if ok, _ := w.Ensure(); !ok || spawned != 2 {
		t.Fatalf("no spawn after forgetting an exited child (%d)", spawned)
	}
	w.Forget()
	if w.Running() != 0 {
		t.Errorf("Running = %d after Forget", w.Running())
	}
}
