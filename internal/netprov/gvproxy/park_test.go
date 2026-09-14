package gvproxy

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// The test binary doubles as a fake ssh and a fake gvproxy, launched through
// a symlink of that name so that the argv checks (forwardAlive, isOurs) see
// the name they look for.
const (
	fakeSSHEnv      = "JM_FAKE_SSH"          // "bind" or "nobind"
	fakeSSHDelayEnv = "JM_FAKE_SSH_DELAY_MS" // before the bind
	fakeGvproxyEnv  = "JM_FAKE_GVPROXY"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeSSHEnv); mode != "" {
		os.Exit(fakeSSHMain(mode, os.Args[1:]))
	}
	if os.Getenv(fakeGvproxyEnv) != "" {
		time.AfterFunc(60*time.Second, func() { os.Exit(9) })
		select {}
	}
	os.Exit(m.Run())
}

// fakeSSHMain serves "-L <local>:<guest podman socket>" the way ssh with
// StreamLocalBindUnlink does: the path is replaced by a new socket.
func fakeSSHMain(mode string, args []string) int {
	time.AfterFunc(60*time.Second, func() { os.Exit(9) })
	var local string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-L" {
			local = strings.TrimSuffix(args[i+1], ":"+machine.GuestPodmanSocket)
		}
	}
	if mode == "nobind" || local == "" {
		select {}
	}
	delay, _ := strconv.Atoi(os.Getenv(fakeSSHDelayEnv))
	time.Sleep(time.Duration(delay) * time.Millisecond)
	ln, err := listenBeside(local)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return 1
		}
		c.Close()
	}
}

// listenBeside binds a socket next to path and renames it over path, so the
// path is never missing, and leaves the file behind when closed.
func listenBeside(path string) (*net.UnixListener, error) {
	tmp := path + ".b"
	_ = os.Remove(tmp)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: tmp, Net: "unix"})
	if err != nil {
		return nil, err
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// shortDir is a machine directory short enough for unix socket paths.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jmgv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// installFake puts a symlink named name to this test binary first on PATH
// and returns its path.
func installFake(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(bin, name)
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return link
}

func mustInode(t *testing.T, path string) uint64 {
	t.Helper()
	ino, ok := socketInode(path)
	if !ok {
		t.Fatalf("%s is not a socket", path)
	}
	return ino
}

// stopHelperOnCleanup makes sure a fake forward never outlives the test.
func stopHelperOnCleanup(t *testing.T, p Paths) {
	t.Cleanup(func() {
		if pid, err := readPID(p.FwdPID); err == nil && procx.Alive(pid) {
			_ = procx.SignalGroup(pid, 9)
		}
	})
}

func TestArgsUseRecordedMTU(t *testing.T) {
	t.Setenv("JM_MTU", "1500")
	m := sampleMachine("/state/machines/test")
	p := PathsFor(m.Dir)
	m.MTU = 4000
	if got := strings.Join(Args(m, p), " "); !strings.Contains(got, "-mtu 4000") {
		t.Fatalf("recorded MTU not used: %s", got)
	}
	m.MTU = 0
	if got := strings.Join(Args(m, p), " "); !strings.Contains(got, "-mtu 1500") {
		t.Fatalf("a record without an MTU must fall back to $JM_MTU: %s", got)
	}
}

// A socket already at the path (a stand-in holding the endpoint) must not
// satisfy the wait: startForward returns only once the helper's own socket
// has replaced it, records that inode, and never removes the old socket when
// the helper fails.
func TestStartForwardWaitsForNewInode(t *testing.T) {
	installFake(t, "ssh")
	t.Setenv(fakeSSHEnv, "bind")
	const delay = 400 * time.Millisecond
	t.Setenv(fakeSSHDelayEnv, strconv.Itoa(int(delay/time.Millisecond)))
	dir := shortDir(t)
	m, p := sampleMachine(dir), PathsFor(dir)
	stopHelperOnCleanup(t, p)

	standin, err := listenBeside(p.Podman)
	if err != nil {
		t.Fatal(err)
	}
	defer standin.Close()
	before := mustInode(t, p.Podman)

	began := time.Now()
	if err := startForward(context.Background(), m, p); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took < delay {
		t.Fatalf("startForward returned after %s, before the helper bound (%s)", took, delay)
	}
	cur := mustInode(t, p.Podman)
	if cur == before {
		t.Fatal("the stand-in's socket satisfied the wait")
	}
	if got, ok := readInode(p.FwdIno); !ok || got != cur {
		t.Fatalf("forward.ino = %d, %v; want %d", got, ok, cur)
	}
	// A second start finds the helper serving its own socket.
	pid, _ := readPID(p.FwdPID)
	if err := startForward(context.Background(), m, p); err != nil {
		t.Fatal(err)
	}
	if again, _ := readPID(p.FwdPID); again != pid {
		t.Fatalf("a serving forward was restarted: pid %d -> %d", pid, again)
	}
	// Stopping without keepSocket removes the helper's own socket.
	if err := stopForward(context.Background(), p, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p.Podman); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the helper's socket was left behind: %v", err)
	}

	// A helper that never binds: the wait fails and the stand-in's socket
	// is untouched.
	t.Setenv(fakeSSHEnv, "nobind")
	saved := forwardTimeout
	forwardTimeout = 300 * time.Millisecond
	t.Cleanup(func() { forwardTimeout = saved })
	standin2, err := listenBeside(p.Podman)
	if err != nil {
		t.Fatal(err)
	}
	defer standin2.Close()
	held := mustInode(t, p.Podman)
	if err := startForward(context.Background(), m, p); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if cur, ok := socketInode(p.Podman); !ok || cur != held || !socketAnswers(p.Podman) {
		t.Fatal("a failed forward removed or replaced the stand-in's socket")
	}
	for _, f := range []string{p.FwdPID, p.FwdIno} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
}

func TestStopForwardKeepSocket(t *testing.T) {
	installFake(t, "ssh")
	t.Setenv(fakeSSHEnv, "bind")
	dir := shortDir(t)
	m, p := sampleMachine(dir), PathsFor(dir)
	stopHelperOnCleanup(t, p)
	ctx := context.Background()

	if err := startForward(ctx, m, p); err != nil {
		t.Fatal(err)
	}
	pid, err := readPID(p.FwdPID)
	if err != nil {
		t.Fatal(err)
	}
	ino := mustInode(t, p.Podman)
	if err := (Provider{}).StopAPIForward(ctx, m); err != nil {
		t.Fatal(err)
	}
	if procx.Alive(pid) {
		t.Fatal("StopAPIForward left the helper running")
	}
	for _, f := range []string{p.FwdPID, p.FwdIno} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
	if cur, ok := socketInode(p.Podman); !ok || cur != ino {
		t.Fatal("StopAPIForward removed podman.sock")
	}

	// Restarting over the stale socket works, and a socket that later
	// replaced the helper's is not removed by a stop.
	if err := startForward(ctx, m, p); err != nil {
		t.Fatal(err)
	}
	foreign, err := listenBeside(p.Podman)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	held := mustInode(t, p.Podman)
	if err := stopForward(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if cur, ok := socketInode(p.Podman); !ok || cur != held || !socketAnswers(p.Podman) {
		t.Fatal("stopForward removed a socket it did not bind")
	}
}

func TestParkLeavesPodmanSocket(t *testing.T) {
	bin := installFake(t, "gvproxy")
	t.Setenv(fakeGvproxyEnv, "1")
	dir := shortDir(t)
	m, p := sampleMachine(dir), PathsFor(dir)
	var pr Provider
	if _, ok := any(pr).(netprov.Parker); !ok {
		t.Fatal("gvproxy provider must implement netprov.Parker")
	}

	pid, err := procx.StartDetached(bin, Args(m, p), nil, p.Log, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if procx.Alive(pid) {
			_ = procx.SignalGroup(pid, 9)
		}
	})
	if err := os.WriteFile(p.PID, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for st, _ := pr.State(m); st != backend.Running; st, _ = pr.State(m) {
		if time.Now().After(deadline) {
			t.Fatalf("fake gvproxy never reads as running: %s", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, f := range []string{p.Net, p.API} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	forwards := filepath.Join(dir, "forwards.json")
	if err := os.WriteFile(forwards, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	standin, err := listenBeside(p.Podman)
	if err != nil {
		t.Fatal(err)
	}
	held := mustInode(t, p.Podman)

	if err := pr.Park(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if procx.Alive(pid) {
		t.Fatal("Park left gvproxy running")
	}
	for _, f := range []string{p.Net, p.API, p.PID} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
	if cur, ok := socketInode(p.Podman); !ok || cur != held {
		t.Fatal("Park removed a podman.sock that is being served")
	}
	if _, err := os.Stat(forwards); err != nil {
		t.Fatalf("Park touched forwards.json: %v", err)
	}
	if st, _ := pr.State(m); st != backend.Stopped {
		t.Fatalf("state after Park = %s", st)
	}

	// Once nothing serves it, a second Park removes the stale file.
	standin.Close()
	if err := pr.Park(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p.Podman); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stale podman.sock was left behind: %v", err)
	}
}
