package qemu

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func sampleMachine() *machine.Machine {
	m := machine.Defaults()
	m.Name = "test"
	return &m
}

func samplePaths(dir string) Paths {
	return Paths{
		Code:    "/opt/homebrew/share/qemu/edk2-aarch64-code.fd",
		Vars:    filepath.Join(dir, machine.EFIVarsFile),
		Disk:    filepath.Join(dir, machine.DiskFile),
		Seed:    filepath.Join(dir, machine.SeedFile),
		Console: filepath.Join(dir, machine.ConsoleFile),
		QMP:     filepath.Join(dir, QMPSockFile),
		PID:     filepath.Join(dir, PIDFile),
		Log:     filepath.Join(dir, LogFile),
	}
}

func TestRegistered(t *testing.T) {
	b, err := backend.Get(Name)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "qemu" || !b.Capabilities().SerialConsole {
		t.Fatalf("unexpected backend %+v", b)
	}
}

func TestArgs(t *testing.T) {
	m := sampleMachine()
	p := samplePaths("/state/machines/test")
	args := Args(m, backend.NetAttachment{Kind: "user", HostFwdSSH: 2222, MAC: m.MAC}, p)

	// Every flag/value pair we expect, in the PoC's order.
	want := [][2]string{
		{"-M", "virt,accel=" + Accel()},
		{"-cpu", "host"},
		{"-smp", "4"},
		{"-m", "4096"},
		{"-drive", "if=pflash,format=raw,readonly=on,file=" + p.Code},
		{"-drive", "if=pflash,format=raw,file=" + p.Vars},
		{"-drive", "file=" + p.Disk + ",format=raw,if=virtio,cache=writeback,discard=unmap"},
		{"-drive", "file=" + p.Seed + ",format=raw,if=virtio,readonly=on"},
		{"-netdev", "user,id=n0,hostfwd=tcp:127.0.0.1:2222-:22"},
		{"-device", "virtio-net-pci,netdev=n0,mac=5a:94:ef:e4:0c:ee"},
		{"-device", "virtio-rng-pci"},
		{"-display", "none"},
		{"-serial", "file:" + p.Console},
		{"-qmp", "unix:" + p.QMP + ",server,nowait"},
		{"-pidfile", p.PID},
	}
	var got [][2]string
	for i := 0; i < len(args)-1; i++ {
		if strings.HasPrefix(args[i], "-") && args[i] != "-daemonize" {
			got = append(got, [2]string{args[i], args[i+1]})
			i++
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
	if !slices.Contains(args, "-daemonize") {
		t.Fatal("missing -daemonize")
	}
	if slices.Contains(args, p.Log) {
		t.Fatal("qemu.log must not be passed to qemu")
	}
}

func TestArgsFallsBackToMachineMAC(t *testing.T) {
	m := sampleMachine()
	args := Args(m, backend.NetAttachment{Kind: "user", HostFwdSSH: 2222}, samplePaths("/x"))
	if !slices.Contains(args, "virtio-net-pci,netdev=n0,mac="+m.MAC) {
		t.Fatalf("machine MAC not used: %q", args)
	}
}

func TestArgsUnknownNetKind(t *testing.T) {
	args := Args(sampleMachine(), backend.NetAttachment{Kind: "vmnet"}, samplePaths("/x"))
	if !slices.Contains(args, "-nic") || slices.Contains(args, "-netdev") {
		t.Fatalf("unknown kind should yield -nic none: %q", args)
	}
}

func TestStateTransitions(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, PIDFile)

	ours := func(pid int) bool { return pid == os.Getpid() }
	check := func(want backend.State) {
		t.Helper()
		got, err := stateFromPIDFile(pidFile, ours)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("state = %s, want %s", got, want)
		}
	}

	check(backend.Stopped) // no pid file

	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(backend.Running) // our own pid, recognised as ours

	if err := os.WriteFile(pidFile, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(backend.Broken) // bogus pid

	if err := os.WriteFile(pidFile, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(backend.Broken) // unparseable pid file
}

// A live pid that is not a qemu started for this pid file (here: the test
// binary itself, standing in for a recycled pid after a reboot) must be
// Broken, never Running, so Stop never signals a foreign process.
func TestStateRejectsForeignPID(t *testing.T) {
	dir := t.TempDir()
	m := sampleMachine()
	m.Dir = dir
	var b Backend
	pidFile := filepath.Join(dir, PIDFile)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if argv := commandLine(os.Getpid()); argv == "" {
		t.Fatalf("commandLine(self) empty; ps unavailable?")
	}
	if isOurQEMU(os.Getpid(), pidFile) {
		t.Fatal("test binary mistaken for qemu")
	}
	if isOurQEMU(2147483646, pidFile) {
		t.Fatal("nonexistent pid mistaken for qemu")
	}
	if st, err := b.State(m); err != nil || st != backend.Broken {
		t.Fatalf("state = %s, %v; want broken", st, err)
	}
	// Stop must repair without touching our own process.
	if err := b.Stop(context.Background(), m, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := b.State(m); st != backend.Stopped {
		t.Fatalf("state after repair = %s, want stopped", st)
	}
}

func TestStateNeedsDir(t *testing.T) {
	var b Backend
	if _, err := b.State(sampleMachine()); err != ErrNoDir {
		t.Fatalf("err = %v, want ErrNoDir", err)
	}
}

func TestStateAndRepairViaBackend(t *testing.T) {
	dir := t.TempDir()
	m := sampleMachine()
	m.Dir = dir
	var b Backend

	if got := b.ConsolePath(m); got != filepath.Join(dir, machine.ConsoleFile) {
		t.Fatalf("ConsolePath = %s", got)
	}
	if logs := b.Logs(m); len(logs) != 2 || logs[0] != filepath.Join(dir, LogFile) || logs[1] != filepath.Join(dir, machine.ConsoleFile) {
		t.Fatalf("Logs = %q", logs)
	}
	pidFile := filepath.Join(dir, PIDFile)
	qmp := QMPSocket(dir)
	for _, f := range []string{pidFile, qmp} {
		if err := os.WriteFile(f, []byte("2147483646\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := b.State(m); st != backend.Broken {
		t.Fatalf("state = %s, want broken", st)
	}
	// Stop on a broken machine repairs it and reports success.
	if err := b.Stop(context.Background(), m, true); err != nil {
		t.Fatal(err)
	}
	if st, _ := b.State(m); st != backend.Stopped {
		t.Fatalf("state after repair = %s, want stopped", st)
	}
	if _, err := os.Stat(qmp); !os.IsNotExist(err) {
		t.Fatal("stale qmp.sock not removed")
	}
	// Stop on a stopped machine is a no-op.
	if err := b.Stop(context.Background(), m, false); err != nil {
		t.Fatal(err)
	}
}

// QMPSocket must never exceed sun_path: a deep --state-root (the e2e run hit
// 133 bytes) falls back to a short, deterministic path outside the machine
// directory.
func TestQMPSocketLength(t *testing.T) {
	short := "/Users/me/.jailmachine/machines/jailmachine"
	if got := QMPSocket(short); got != filepath.Join(short, QMPSockFile) {
		t.Fatalf("short dir: %s", got)
	}
	long := "/private/tmp/claude-501/-Users-belli-Projects-jailmachine/ec17a1da-d007-4581-940f-e4b6a0fa70e6/scratchpad/state/machines/e2e"
	if len(filepath.Join(long, QMPSockFile)) <= MaxSocketPath {
		t.Fatal("test fixture is not long enough")
	}
	got := QMPSocket(long)
	if len(got) > MaxSocketPath {
		t.Fatalf("fallback still too long (%d): %s", len(got), got)
	}
	if strings.HasPrefix(got, long) {
		t.Fatalf("fallback must leave the machine dir: %s", got)
	}
	if again := QMPSocket(long); again != got {
		t.Fatalf("not deterministic: %s vs %s", got, again)
	}
	if QMPSocket(long+"2") == got {
		t.Fatal("different dirs must not share a socket")
	}
	// The backend's paths use the same rule.
	m := sampleMachine()
	m.Dir = long
	if p := (Backend{}).paths(m); p.QMP != got {
		t.Fatalf("paths().QMP = %s, want %s", p.QMP, got)
	}
}

// fakeQMP serves one connection on a unix socket, speaking just enough QMP
// to record the commands it receives.
func fakeQMP(t *testing.T) (sock string, got <-chan []string) {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes on macOS; t.TempDir can
	// exceed that, so use the system temp dir directly.
	dir, err := os.MkdirTemp("", "jmqmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "qmp.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	ch := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- nil
			return
		}
		defer conn.Close()
		enc := json.NewEncoder(conn)
		dec := json.NewDecoder(conn)
		_ = enc.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []string{}}})
		var cmds []string
		for {
			var c qmpCommand
			if err := dec.Decode(&c); err != nil {
				break
			}
			cmds = append(cmds, c.Execute)
			// Interleave an event to make sure the client skips it.
			_ = enc.Encode(map[string]any{"event": "POWERDOWN", "timestamp": map[string]int{"seconds": 0}})
			_ = enc.Encode(map[string]any{"return": map[string]any{}})
		}
		ch <- cmds
	}()
	return sock, ch
}

func TestPowerdownQMP(t *testing.T) {
	sock, got := fakeQMP(t)
	if err := Powerdown(context.Background(), sock); err != nil {
		t.Fatal(err)
	}
	want := []string{"qmp_capabilities", "system_powerdown"}
	if cmds := <-got; !reflect.DeepEqual(cmds, want) {
		t.Fatalf("server saw %q, want %q", cmds, want)
	}
}

func TestPowerdownNoSocket(t *testing.T) {
	if err := Powerdown(context.Background(), filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("expected error for missing socket")
	}
}

func TestFirmwareDir(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin", Binary)
	share := filepath.Join(prefix, "share", "qemu")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, FirmwareCode), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FirmwareDir(bin)
	if err != nil {
		t.Fatal(err)
	}
	if got != share {
		t.Fatalf("FirmwareDir = %s, want %s", got, share)
	}
}

func TestEnsureEFIVars(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tmpl.fd")
	dst := filepath.Join(dir, "m", machine.EFIVarsFile)
	if err := os.WriteFile(src, []byte("vars"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureEFIVars(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureEFIVars(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "modified" {
		t.Fatal("existing efivars.fd must not be overwritten")
	}
}
