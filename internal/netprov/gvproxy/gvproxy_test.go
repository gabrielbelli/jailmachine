package gvproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

func sampleMachine(dir string) *machine.Machine {
	m := machine.Defaults()
	m.Name = "test"
	m.Dir = dir
	return &m
}

func TestRegistered(t *testing.T) {
	p, err := netprov.Get(Name)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "gvproxy" {
		t.Fatalf("name = %s", p.Name())
	}
	if _, ok := p.(backend.Cleaner); !ok {
		t.Fatal("gvproxy provider must implement backend.Cleaner")
	}
}

func TestArgs(t *testing.T) {
	dir := "/state/machines/test"
	m := sampleMachine(dir)
	p := PathsFor(dir)
	got := Args(m, p)
	want := []string{
		"-listen-qemu", "unix:///state/machines/test/net.sock",
		"-listen", "unix:///state/machines/test/api.sock",
		"-ssh-port", "2222",
		"-pid-file", "/state/machines/test/gvproxy.pid",
		"-log-file", "/state/machines/test/gvproxy.log",
		"-mtu", strconv.Itoa(DefaultMTU),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
	// gvproxy's own -forward-sock kills the whole process when guest sshd
	// is slow; the podman socket is forwarded by a separate helper.
	if strings.Contains(strings.Join(got, " "), "-forward") {
		t.Fatalf("argv must not use gvproxy's -forward-* flags: %q", got)
	}
	if _, ok := any(Provider{}).(netprov.APIForwarder); !ok {
		t.Fatal("gvproxy provider must implement netprov.APIForwarder")
	}
	if !(Provider{}).Capabilities().Supervised {
		t.Fatal("gvproxy must report Supervised")
	}
}

// A forward pid file pointing at a non-ssh process (this test binary) or
// a dead pid is not a live forwarder; stopForward tidies it without
// signalling anything.
func TestForwardLiveness(t *testing.T) {
	dir := t.TempDir()
	p := PathsFor(dir)
	if err := os.WriteFile(p.FwdPID, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := forwardAlive(p); ok {
		t.Fatal("test binary mistaken for the ssh forwarder")
	}
	if err := os.WriteFile(p.Podman, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stopForward(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p.FwdPID, p.Podman} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s not removed", f)
		}
	}
	if err := os.WriteFile(p.FwdPID, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := forwardAlive(p); ok {
		t.Fatal("dead pid mistaken for the ssh forwarder")
	}
	// Repair on a broken provider also drops the forwarder's pid file.
	var pr Provider
	if err := pr.Repair(sampleMachine(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.FwdPID); !os.IsNotExist(err) {
		t.Fatal("Repair left forward.pid behind")
	}
	if logs := pr.Logs(sampleMachine(dir)); len(logs) != 2 || logs[1] != p.FwdLog {
		t.Fatalf("Logs = %q", logs)
	}
}

func TestEndpointAndAttachment(t *testing.T) {
	dir := "/state/machines/test"
	m := sampleMachine(dir)
	var pr Provider
	ep, err := pr.Endpoint(m)
	if err != nil {
		t.Fatal(err)
	}
	want := netprov.Endpoint{GuestIP: "192.168.127.2", SSHHost: "127.0.0.1", SSHPort: 2222, APISocket: dir + "/podman.sock", DNS: "192.168.127.2", Gateway: "192.168.127.1", HostAlias: "192.168.127.254"}
	if ep != want {
		t.Fatalf("endpoint = %+v, want %+v", ep, want)
	}
	att := attachment(m, PathsFor(dir))
	if att.Kind != backend.KindStream || att.SocketPath != dir+"/net.sock" || att.MAC != m.MAC {
		t.Fatalf("attachment = %+v", att)
	}
	if _, err := pr.Endpoint(sampleMachine("")); err != ErrNoDir {
		t.Fatalf("no dir: %v", err)
	}
	if logs := pr.Logs(m); len(logs) != 2 || logs[0] != dir+"/gvproxy.log" {
		t.Fatalf("Logs = %q", logs)
	}
}

// Sockets reuse the shared sun_path fallback: a deep state root moves all
// three out of tree, each to a distinct path, and Cleanup removes them.
func TestSocketFallbackAndCleanup(t *testing.T) {
	long := "/private/tmp/claude-501/-Users-belli-Projects-jailmachine/ec17a1da-d007-4581-940f-e4b6a0fa70e6/scratchpad/state/machines/e2e"
	p := PathsFor(long)
	seen := map[string]bool{}
	for _, s := range p.Sockets() {
		if len(s) > backend.MaxSocketPath {
			t.Fatalf("socket too long (%d): %s", len(s), s)
		}
		if strings.HasPrefix(s, long) {
			t.Fatalf("socket must leave the machine dir: %s", s)
		}
		if seen[s] {
			t.Fatalf("duplicate socket path %s", s)
		}
		seen[s] = true
		if err := os.WriteFile(s, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var pr Provider
	if err := pr.Cleanup(sampleMachine(long)); err != nil {
		t.Fatal(err)
	}
	for s := range seen {
		if _, err := os.Stat(s); !os.IsNotExist(err) {
			t.Fatalf("out-of-tree socket %s not removed", s)
		}
	}
	// In-tree sockets are left to the directory removal.
	dir := t.TempDir()
	inTree := PathsFor(dir).Net
	if err := os.WriteFile(inTree, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pr.Cleanup(sampleMachine(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inTree); err != nil {
		t.Fatal("in-tree socket must be left alone")
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
	check(backend.Stopped)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(backend.Running)
	if err := os.WriteFile(pidFile, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(backend.Broken)
	if err := os.WriteFile(pidFile, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	check(backend.Broken)
}

// A live pid that is not a gvproxy started for our api.sock (the test
// binary, standing in for a recycled pid) is Broken; Stop repairs without
// signalling it.
func TestStateRejectsForeignPID(t *testing.T) {
	dir := t.TempDir()
	m := sampleMachine(dir)
	p := PathsFor(dir)
	var pr Provider
	if err := os.WriteFile(p.PID, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if commandLine(os.Getpid()) == "" {
		t.Fatal("commandLine(self) empty; ps unavailable?")
	}
	if isOurs(os.Getpid(), p.API) {
		t.Fatal("test binary mistaken for gvproxy")
	}
	if isOurs(2147483646, p.API) {
		t.Fatal("nonexistent pid mistaken for gvproxy")
	}
	if st, err := pr.State(m); err != nil || st != backend.Broken {
		t.Fatalf("state = %s, %v; want broken", st, err)
	}
	for _, s := range p.Sockets() {
		if err := os.WriteFile(s, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pr.Stop(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if st, _ := pr.State(m); st != backend.Stopped {
		t.Fatalf("state after repair = %s", st)
	}
	for _, s := range append(p.Sockets(), p.PID) {
		if _, err := os.Stat(s); !os.IsNotExist(err) {
			t.Fatalf("stale %s not removed", s)
		}
	}
	// Stop on a stopped provider is a no-op.
	if err := pr.Stop(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := pr.State(sampleMachine("")); err != ErrNoDir {
		t.Fatalf("no dir: %v", err)
	}
}

// fakeAPI serves gvproxy's forwarder API on a unix socket, recording the
// bodies it receives and serving the resulting table on /all.
type fakeAPI struct {
	mu     sync.Mutex
	expose []wire
	unexp  []wire
	table  []wire
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	decode := func(w http.ResponseWriter, r *http.Request) (wire, bool) {
		var in wire
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || json.Unmarshal(body, &in) != nil || in.Local == "" {
			http.Error(w, "bad request: "+string(body), http.StatusBadRequest)
			return in, false
		}
		return in, true
	}
	mux.HandleFunc(exposePath, func(w http.ResponseWriter, r *http.Request) {
		in, ok := decode(w, r)
		if !ok {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, e := range f.table {
			if e == in {
				http.Error(w, "port already exposed", http.StatusInternalServerError)
				return
			}
		}
		f.expose = append(f.expose, in)
		f.table = append(f.table, in)
	})
	mux.HandleFunc(unexposePath, func(w http.ResponseWriter, r *http.Request) {
		in, ok := decode(w, r)
		if !ok {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unexp = append(f.unexp, in)
		kept := f.table[:0]
		for _, e := range f.table {
			if e != in {
				kept = append(kept, e)
			}
		}
		f.table = kept
	})
	mux.HandleFunc(listPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.table)
	})
	return mux
}

func startFakeAPI(t *testing.T) (string, *fakeAPI) {
	t.Helper()
	// t.TempDir can exceed sun_path on macOS; use the system temp dir.
	dir, err := os.MkdirTemp("", "jmgv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, APISockFile)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAPI{}
	srv := httptest.NewUnstartedServer(f.handler())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return sock, f
}

func TestClientExposeUnexposeList(t *testing.T) {
	sock, f := startFakeAPI(t)
	c := NewClient(sock)
	ctx := context.Background()

	a := netprov.Mapping{Proto: "tcp", Local: "127.0.0.1:8080", Remote: "192.168.127.2:80"}
	b := netprov.Mapping{Local: "127.0.0.1:5353", Remote: "192.168.127.2:53", Proto: "udp"}
	if err := c.Expose(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.Expose(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := c.Expose(ctx, a); err == nil || !strings.Contains(err.Error(), "already exposed") {
		t.Fatalf("duplicate expose should surface the server error, got %v", err)
	}
	got, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []netprov.Mapping{a, b}) {
		t.Fatalf("List = %+v", got)
	}
	if err := c.Unexpose(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err = c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []netprov.Mapping{b}) {
		t.Fatalf("List after unexpose = %+v", got)
	}
	wantExp := []wire{{Local: "127.0.0.1:8080", Remote: "192.168.127.2:80", Protocol: "tcp"}, {Local: "127.0.0.1:5353", Remote: "192.168.127.2:53", Protocol: "udp"}}
	if !reflect.DeepEqual(f.expose, wantExp) {
		t.Fatalf("server saw expose %+v, want %+v", f.expose, wantExp)
	}
	if !reflect.DeepEqual(f.unexp, wantExp[:1]) {
		t.Fatalf("server saw unexpose %+v", f.unexp)
	}
}

// An empty Proto defaults to tcp on the wire, and the Provider methods route
// through the machine's api.sock.
func TestProviderMappingViaMachineDir(t *testing.T) {
	sock, f := startFakeAPI(t)
	dir := filepath.Dir(sock)
	m := sampleMachine(dir)
	var pr Provider
	ctx := context.Background()
	mp := netprov.Mapping{Local: "127.0.0.1:9000", Remote: "192.168.127.2:9000"}
	if err := pr.Expose(ctx, m, mp); err != nil {
		t.Fatal(err)
	}
	if len(f.expose) != 1 || f.expose[0].Protocol != "tcp" {
		t.Fatalf("proto not defaulted: %+v", f.expose)
	}
	got, err := pr.List(ctx, m)
	if err != nil || len(got) != 1 || got[0].Proto != "tcp" {
		t.Fatalf("List = %+v, %v", got, err)
	}
	if err := pr.Unexpose(ctx, m, mp); err != nil {
		t.Fatal(err)
	}
	if got, _ := pr.List(ctx, m); len(got) != 0 {
		t.Fatalf("table not emptied: %+v", got)
	}
	if err := pr.Expose(ctx, sampleMachine(""), mp); err != ErrNoDir {
		t.Fatalf("no dir: %v", err)
	}
}

func TestClientNoSocket(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("expected error for missing socket")
	}
}

func TestLookupBinaryEnv(t *testing.T) {
	t.Setenv(BinaryEnv, filepath.Join(t.TempDir(), "nope"))
	if _, err := LookupBinary(); err == nil {
		t.Fatal("bogus $JM_GVPROXY must fail")
	}
	fake := filepath.Join(t.TempDir(), "gvproxy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, fake)
	if bin, err := LookupBinary(); err != nil || bin != fake {
		t.Fatalf("LookupBinary = %s, %v", bin, err)
	}
}

// Start must refuse without launching anything when the ssh identity is
// missing, and leave no runtime files behind.
func TestStartNeedsIdentity(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "gvproxy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, fake)
	dir, err := os.MkdirTemp("", "jmgv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	var pr Provider
	_, _, err = pr.Start(context.Background(), sampleMachine(dir))
	if err == nil || !strings.Contains(err.Error(), "ssh identity missing") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(PathsFor(dir).PID); !os.IsNotExist(err) {
		t.Fatal("pid file must not exist")
	}
}

// A binary that exits without creating its sockets is reported with its
// log tail, and nothing is left running or on disk.
func TestStartReportsEarlyExit(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "gvproxy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BinaryEnv, fake)
	dir, err := os.MkdirTemp("", "jmgv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.MkdirAll(filepath.Join(dir, machine.SSHDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, machine.SSHKeyFile), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pr Provider
	m := sampleMachine(dir)
	_, _, err = pr.Start(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "exited before creating") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := pr.State(m); st != backend.Stopped {
		t.Fatalf("state after failed start = %s", st)
	}
}

// A host port another process already holds is the one expose failure a user
// can do something about, so it must not reach "jm ports" as raw control-API
// plumbing. Every other failure is passed through untouched: rewriting an
// error nobody has a fix for only hides what actually went wrong.
func TestExposeErrExplainsABusyHostPort(t *testing.T) {
	raw := errors.New("gvproxy api: POST /services/forwarder/expose: 500 Internal Server Error: " +
		"listen udp 0.0.0.0:5353: bind: address already in use")
	udp := netprov.Mapping{Proto: "udp", Local: "0.0.0.0:5353", Remote: "192.168.127.2:5353"}
	got := exposeErr(udp, raw)
	if got == nil {
		t.Fatal("a busy host port must stay an error")
	}
	for _, want := range []string{"already holds this host port", "lsof -nP -iUDP:5353", "different host port"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("message %q does not mention %q", got, want)
		}
	}
	if strings.Contains(got.Error(), "gvproxy api") {
		t.Errorf("message still leaks the control API: %q", got)
	}

	// An empty Proto is tcp, as it is on the wire.
	tcp := netprov.Mapping{Local: "127.0.0.1:8080", Remote: "192.168.127.2:8080"}
	if got := exposeErr(tcp, raw); !strings.Contains(got.Error(), "lsof -nP -iTCP:8080") {
		t.Errorf("empty proto should read as tcp, got %q", got)
	}

	if err := exposeErr(udp, nil); err != nil {
		t.Errorf("success must stay success, got %v", err)
	}
	other := errors.New("gvproxy api: POST /services/forwarder/expose: 500: port already exposed")
	if got := exposeErr(udp, other); got.Error() != other.Error() {
		t.Errorf("unrelated failure was rewritten to %q", got)
	}
}

func TestMTUFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", DefaultMTU},
		{"nonsense", DefaultMTU},
		{"1500", 1500},
		{"576", 576},
		{"100", DefaultMTU}, // below the floor: ignored
		{"70000", MaxMTU},   // above virtio's jumbo limit: clamped
		{" 4000 ", 4000},    // trimmed
	} {
		t.Setenv("JM_MTU", tc.env)
		if got := MTU(); got != tc.want {
			t.Errorf("JM_MTU=%q: MTU() = %d, want %d", tc.env, got, tc.want)
		}
	}
}
