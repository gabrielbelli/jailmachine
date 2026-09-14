package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/procx"
)

// fakeQEMUEnv switches the test binary into a fake qemu-system-aarch64 (see
// TestMain): "ready" writes the pid file at once and answers QMP after
// $JM_FAKE_QEMU_DELAY_MS, "exit" fails at once, "hang" never gets ready,
// "unsupported" fails the way QEMU does on an unknown machine type. The QMP
// side is serveFakeQEMU (fake_qemu_test.go).
const (
	fakeQEMUEnv      = "JM_FAKE_QEMU"
	fakeQEMUDelayEnv = "JM_FAKE_QEMU_DELAY_MS"
	fakeQEMUSelfEnv  = "JM_FAKE_QEMU_SELF" // where the shim records its pid
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeQEMUEnv); mode != "" {
		os.Exit(fakeQEMUMain(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// optValue reads a QEMU option value up to the first single comma, turning
// ",," back into ",".
func optValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if i+1 < len(s) && s[i+1] == ',' {
				b.WriteByte(',')
				i++
				continue
			}
			break
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func fakeQEMUMain(mode string, args []string) int {
	// Never outlive a broken test run.
	time.AfterFunc(60*time.Second, func() { os.Exit(9) })
	var pidFile, qmp string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-pidfile":
			pidFile = args[i+1]
		case "-qmp":
			qmp = optValue(strings.TrimPrefix(args[i+1], "unix:"))
		}
	}
	switch mode {
	case "exit":
		fmt.Fprintln(os.Stderr, "qemu-system-aarch64: boom: fake failure")
		return 1
	case "hang":
		select {}
	}
	return serveFakeQEMU(mode, args, pidFile, qmp)
}

// fakeQEMUInstall puts a qemu-system-aarch64 script that execs this test
// binary first on PATH, with firmware beside it, and returns a machine whose
// directory is ready to boot.
func fakeQEMUInstall(t *testing.T, mode string) (*machine.Machine, Paths) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin", Binary)
	share := filepath.Join(prefix, "share", "qemu")
	for _, d := range []string{filepath.Dir(bin), share} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The shim records its pid before the exec (which keeps it), so a test
	// that kills the launch early does not depend on how fast the Go test
	// binary starts.
	script := "#!/bin/sh\n[ -n \"$" + fakeQEMUSelfEnv + "\" ] && echo $$ > \"$" + fakeQEMUSelfEnv + "\"\nexec '" + self + "' \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{FirmwareCode, FirmwareVars} {
		if err := os.WriteFile(filepath.Join(share, f), []byte("fw"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeQEMUEnv, mode)

	m := sampleMachine()
	m.Dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(m.Dir, machine.DiskFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return m, Backend{}.paths(m)
}

func killPID(t *testing.T, pid int) {
	t.Helper()
	if pid > 0 && procx.Alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		procx.WaitExit(context.Background(), pid, 5*time.Second)
	}
}

func TestStartWaitsForQMPReadiness(t *testing.T) {
	m, p := fakeQEMUInstall(t, "ready")
	const delay = 400 * time.Millisecond
	t.Setenv(fakeQEMUDelayEnv, strconv.Itoa(int(delay/time.Millisecond)))
	if err := os.WriteFile(p.Console, []byte("previous boot\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	began := time.Now()
	if err := (Backend{}).Start(context.Background(), m, backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222}); err != nil {
		t.Fatal(err)
	}
	pid, err := readPID(p.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killPID(t, pid) })
	// The pid file appears at once; only the QMP answer makes it ready.
	if took := time.Since(began); took < delay {
		t.Fatalf("Start returned after %s, before QMP answered (%s)", took, delay)
	}
	if !procx.Alive(pid) {
		t.Fatal("launched QEMU is not alive")
	}
	if data, err := os.ReadFile(p.Console); err != nil || len(data) != 0 {
		t.Fatalf("console.log not truncated: %q, %v", data, err)
	}
	argv, err := ReadArgv(m.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) == 0 || filepath.Base(argv[0]) != Binary {
		t.Fatalf("qemu.argv[0] = %q", argv)
	}
	if slices.Contains(argv, "-daemonize") || !slices.Contains(argv, "chardev:"+ConsoleChardevID) {
		t.Fatalf("qemu.argv = %q", argv)
	}
	if got := commandLine(pid); !strings.Contains(got, "-pidfile "+p.PID) {
		t.Fatalf("the recorded pid is not the launched process: %q", got)
	}
	if st, err := os.Stat(filepath.Join(m.Dir, ArgvFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("qemu.argv: %v, %v", st, err)
	}
	entries, _ := os.ReadDir(m.Dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestStartReportsEarlyExitWithLogTail(t *testing.T) {
	m, p := fakeQEMUInstall(t, "exit")
	err := (Backend{}).Start(context.Background(), m, backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222})
	if err == nil || !strings.Contains(err.Error(), "exited during startup") || !strings.Contains(err.Error(), "boom: fake failure") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := (Backend{}).State(m); st != backend.Stopped {
		t.Fatalf("state after a failed start = %s, want stopped", st)
	}
	if _, err := os.Stat(p.PID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file left behind: %v", err)
	}
}

func TestStartKillsAHungLaunch(t *testing.T) {
	m, p := fakeQEMUInstall(t, "hang")
	selfFile := filepath.Join(t.TempDir(), "hang.pid")
	t.Setenv(fakeQEMUSelfEnv, selfFile)
	saved := launchReadyTimeout
	launchReadyTimeout = 500 * time.Millisecond
	t.Cleanup(func() { launchReadyTimeout = saved })

	err := (Backend{}).Start(context.Background(), m, backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222})
	if err == nil || !strings.Contains(err.Error(), "not ready after") {
		t.Fatalf("err = %v", err)
	}
	data, rerr := os.ReadFile(selfFile)
	if rerr != nil {
		t.Fatalf("the fake never ran: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	t.Cleanup(func() { killPID(t, pid) })
	if procx.Alive(pid) {
		t.Fatalf("hung QEMU %d left running", pid)
	}
	for _, f := range []string{p.PID, p.QMP} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(m.Dir, ArgvFile)); err != nil {
		t.Fatalf("qemu.argv not written: %v", err)
	}
}

// A launch that cannot be killed stays recorded in the pid file, so State
// does not report Stopped while a QEMU still holds the disk.
func TestStartKeepsTrackOfAnUnkillableLaunch(t *testing.T) {
	m, p := fakeQEMUInstall(t, "hang")
	selfFile := filepath.Join(t.TempDir(), "hang.pid")
	t.Setenv(fakeQEMUSelfEnv, selfFile)
	savedTimeout, savedKill, savedKillTimeout := launchReadyTimeout, launchKill, launchKillTimeout
	launchReadyTimeout = 500 * time.Millisecond
	launchKill = func(int) error { return nil }
	launchKillTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		launchReadyTimeout, launchKill, launchKillTimeout = savedTimeout, savedKill, savedKillTimeout
	})

	err := (Backend{}).Start(context.Background(), m, backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222})
	data, rerr := os.ReadFile(selfFile)
	if rerr != nil {
		t.Fatalf("the fake never ran: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	t.Cleanup(func() { killPID(t, pid) })
	if err == nil || !strings.Contains(err.Error(), "did not exit after SIGKILL") {
		t.Fatalf("err = %v", err)
	}
	if got, err := readPID(p.PID); err != nil || got != pid {
		t.Fatalf("pid file = %d, %v; want %d", got, err, pid)
	}
	// A real QEMU reads as running; the fake's argv names the test binary,
	// so it reads as broken. Either way it must not read as stopped.
	if st, _ := (Backend{}).State(m); st == backend.Stopped {
		t.Fatal("state with the launch still alive = stopped")
	}
}

// Every network kind, with and without shares, keeps -pidfile and never
// daemonises.
func TestArgsHasNoDaemonize(t *testing.T) {
	p := samplePaths("/state/machines/test")
	p.GuestConf = "/state/machines/test/guest"
	nets := []backend.NetAttachment{
		{Kind: backend.KindUser, HostFwdSSH: 2222},
		{Kind: backend.KindStream, SocketPath: "/state/machines/test/net.sock"},
		{Kind: "vmnet"},
	}
	for _, m := range []*machine.Machine{sampleMachine(), shareMachine(t, "/Volumes", "/private/tmp:ro")} {
		for _, n := range nets {
			args := Args(m, n, p)
			if slices.Contains(args, "-daemonize") {
				t.Fatalf("%s with %d shares daemonises: %q", n.Kind, len(m.Shares), args)
			}
			if i := slices.Index(args, "-pidfile"); i < 0 || args[i+1] != p.PID {
				t.Fatalf("%s: -pidfile missing: %q", n.Kind, args)
			}
		}
	}
}

func TestArgsConsoleChardevAppends(t *testing.T) {
	p := samplePaths("/state/ma,chines/test")
	args := Args(sampleMachine(), backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222}, p)
	got := pairs(args, "-chardev", "-serial")
	want := [][2]string{
		{"-chardev", "file,id=jmcon,path=/state/ma,,chines/test/console.log,append=on"},
		{"-serial", "chardev:jmcon"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("console argv\n got: %q\nwant: %q", got, want)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "file:") {
			t.Fatalf("old -serial file: console left in argv: %q", args)
		}
	}
}

// scriptedQMP serves one connection: the greeting, then for each command
// the frames reply returns, recording the commands it saw.
func scriptedQMP(t *testing.T, reply func(c qmpCommand) []any) (sock string, seen <-chan []qmpCommand) {
	t.Helper()
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
	ch := make(chan []qmpCommand, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- nil
			return
		}
		defer conn.Close()
		enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
		_ = enc.Encode(map[string]any{"QMP": map[string]any{"version": map[string]any{}, "capabilities": []string{}}})
		var cmds []qmpCommand
		for {
			var c qmpCommand
			if dec.Decode(&c) != nil {
				break
			}
			cmds = append(cmds, c)
			for _, f := range reply(c) {
				_ = enc.Encode(f)
			}
		}
		ch <- cmds
	}()
	return sock, ch
}

func ret(v any) map[string]any { return map[string]any{"return": v} }

var qmpEvent = map[string]any{"event": "RESUME", "timestamp": map[string]int{"seconds": 1}}

func TestMonitorSkipsEventsAndDecodesReturn(t *testing.T) {
	sock, seen := scriptedQMP(t, func(c qmpCommand) []any {
		switch c.Execute {
		case "query-status":
			return []any{qmpEvent, qmpEvent, ret(map[string]any{"status": "running", "running": true})}
		case "query-version":
			return []any{qmpEvent, ret(map[string]any{"qemu": map[string]int{"major": 11, "minor": 1, "micro": 1}, "package": ""})}
		case "query-machines":
			return []any{ret([]map[string]any{{"name": "virt-11.0"}, {"name": "virt-11.1", "alias": "virt"}})}
		case "query-migrate-capabilities":
			return []any{ret([]map[string]any{{"capability": "multifd", "state": false}, {"capability": "mapped-ram", "state": false}})}
		case "query-migrate":
			return []any{qmpEvent, ret(map[string]any{"status": "setup", "blocked-reasons": []string{"9p mounted"}, "total-time": 12, "ram": map[string]int{"transferred": 5, "total": 9}})}
		}
		return []any{qmpEvent, ret(map[string]any{})}
	})
	ctx := context.Background()
	q, err := DialMonitor(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := q.QueryStatus(ctx); err != nil || st != "running" {
		t.Fatalf("QueryStatus = %q, %v", st, err)
	}
	if v, err := q.QueryVersion(ctx); err != nil || v != "11.1.1" {
		t.Fatalf("QueryVersion = %q, %v", v, err)
	}
	if mt, err := q.AliasTarget(ctx, "virt"); err != nil || mt != "virt-11.1" {
		t.Fatalf("AliasTarget = %q, %v", mt, err)
	}
	if _, err := q.AliasTarget(ctx, "nope"); err == nil {
		t.Fatal("AliasTarget of an unknown alias must fail")
	}
	if err := q.FileMigrationCapable(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := q.QueryMigrate(ctx)
	if err != nil || info.Status != "setup" || !reflect.DeepEqual(info.BlockedReasons, []string{"9p mounted"}) || info.TotalTime != 12 || info.RAM == nil || info.RAM.Transferred != 5 {
		t.Fatalf("QueryMigrate = %+v, %v", info, err)
	}
	if err := q.SetFileMigration(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if err := q.Migrate(ctx, "file:/x/suspend.state.tmp"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func(context.Context) error{q.Stop, q.Cont, q.MigrateCancel, q.Quit} {
		if err := f(ctx); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()

	var names []string
	var setCaps, setParams, migrate any
	for _, c := range <-seen {
		names = append(names, c.Execute)
		switch c.Execute {
		case "migrate-set-capabilities":
			setCaps = c.Arguments
		case "migrate-set-parameters":
			setParams = c.Arguments
		case "migrate":
			migrate = c.Arguments
		}
	}
	want := []string{"qmp_capabilities", "query-status", "query-version", "query-machines", "query-machines",
		"query-migrate-capabilities", "query-migrate", "migrate-set-capabilities", "migrate-set-parameters",
		"migrate", "stop", "cont", "migrate_cancel", "quit"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("server saw %q\nwant %q", names, want)
	}
	if b, _ := json.Marshal(setCaps); string(b) != `{"capabilities":[{"capability":"mapped-ram","state":true},{"capability":"multifd","state":true}]}` {
		t.Errorf("migrate-set-capabilities arguments = %s", b)
	}
	if b, _ := json.Marshal(setParams); string(b) != `{"multifd-channels":4}` {
		t.Errorf("migrate-set-parameters arguments = %s", b)
	}
	if b, _ := json.Marshal(migrate); string(b) != `{"uri":"file:/x/suspend.state.tmp"}` {
		t.Errorf("migrate arguments = %s", b)
	}
}

func TestMonitorQMPError(t *testing.T) {
	sock, _ := scriptedQMP(t, func(c qmpCommand) []any {
		switch c.Execute {
		case "cont":
			return []any{qmpEvent, map[string]any{"error": map[string]any{"class": "GenericError", "desc": "Migration is not finalized"}}}
		case "query-migrate-capabilities":
			return []any{ret([]map[string]any{{"capability": "multifd", "state": false}})}
		}
		return []any{ret(map[string]any{"status": "paused"})}
	})
	ctx := context.Background()
	q, err := DialMonitor(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	err = q.Cont(ctx)
	var qe *QMPError
	if !errors.As(err, &qe) || qe.Class != "GenericError" || qe.Desc != "Migration is not finalized" {
		t.Fatalf("Cont err = %v (%T)", err, err)
	}
	if !strings.Contains(err.Error(), "cont") {
		t.Errorf("error does not name the command: %v", err)
	}
	// The session survives an error reply.
	if st, err := q.QueryStatus(ctx); err != nil || st != "paused" {
		t.Fatalf("after an error: %q, %v", st, err)
	}
	if err := q.FileMigrationCapable(ctx); err == nil || !strings.Contains(err.Error(), "mapped-ram") {
		t.Fatalf("missing mapped-ram not reported: %v", err)
	}
}

// A cancelled context interrupts a call that is waiting for a reply.
func TestMonitorCallHonoursContext(t *testing.T) {
	sock, _ := scriptedQMP(t, func(c qmpCommand) []any {
		if c.Execute == "qmp_capabilities" {
			return []any{ret(map[string]any{})}
		}
		return nil // never answer
	})
	q, err := DialMonitor(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	if _, err := q.QueryStatus(ctx); err == nil {
		t.Fatal("a call with no reply must fail")
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("call ignored the context for %s", took)
	}
}
