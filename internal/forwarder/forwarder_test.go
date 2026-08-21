package forwarder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

const guest = "192.168.127.2"

// psJSON is "podman ps --format json" as podman 5.x/6.x prints it, trimmed
// to the fields around Ports: one container with two ports (one of them
// bound to a host address), one with a range, one exited (ps -a) and one
// without ports.
const psJSON = `[
  {"AutoRemove":false,"Command":["nginx","-g","daemon off;"],"Created":"2026-08-20T10:00:00Z","Exited":false,"Id":"a1","Image":"docker.io/library/nginx:alpine",
   "Names":["web"],"Pid":1234,"Ports":[
     {"host_ip":"","container_port":80,"host_port":8080,"range":1,"protocol":"tcp"},
     {"host_ip":"0.0.0.0","container_port":443,"host_port":8443,"range":1,"protocol":"tcp"}
   ],"State":"running","Status":"Up 5 minutes"},
  {"Exited":false,"Id":"b2","Names":["udp"],"Ports":[
     {"host_ip":"","container_port":5000,"host_port":6000,"range":3,"protocol":"udp"}
   ],"State":"running"},
  {"Exited":true,"Id":"c3","Names":["old"],"Ports":[
     {"host_ip":"","container_port":80,"host_port":9090,"range":1,"protocol":"tcp"}
   ],"State":"exited"},
  {"Exited":false,"Id":"d4","Names":["quiet"],"Ports":null,"State":"running"},
  {"Exited":false,"Id":"e5","Names":["dup"],"Ports":[
     {"host_ip":"","container_port":81,"host_port":8080,"range":1,"protocol":"tcp"}
   ],"State":"running"},
  {"Exited":false,"Id":"f6","Names":["lo"],"Ports":[
     {"host_ip":"127.0.0.1","container_port":80,"host_port":7070,"range":1,"protocol":"tcp"},
     {"host_ip":"192.168.127.2","container_port":80,"host_port":7071,"range":1,"protocol":"tcp"}
   ],"State":"running"}
]`

func TestDesired(t *testing.T) {
	got, err := Desired([]byte(psJSON), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	// Every reachable port binds every host interface, as docker does on
	// Linux: "-p 8080:80" is reachable at 127.0.0.1, ::1 and the host's
	// LAN address. The range becomes one mapping per port, udp keeps its
	// protocol, and the duplicate host port is published once.
	want := []netprov.Mapping{
		{Proto: "tcp", Local: "0.0.0.0:7071", Remote: guest + ":7071"},
		{Proto: "tcp", Local: "0.0.0.0:8080", Remote: guest + ":8080"},
		{Proto: "udp", Local: "0.0.0.0:6000", Remote: guest + ":6000"},
		{Proto: "udp", Local: "0.0.0.0:6001", Remote: guest + ":6001"},
		{Proto: "udp", Local: "0.0.0.0:6002", Remote: guest + ":6002"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Desired =\n%v\nwant\n%v", got, want)
	}
	// A published port that names a host address is bound inside the guest
	// (or, for the literal wildcard, redirected to an address no packet
	// carries): not exposed, reported with a reason instead.
	_, skipped, err := Plan([]byte(psJSON), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 2 {
		t.Fatalf("Plan skipped = %+v", skipped)
	}
	if skipped[0].Local != "0.0.0.0:8443" || skipped[0].Remote != "" ||
		!strings.Contains(skipped[0].Error, "never matches") ||
		!strings.Contains(skipped[0].Error, "-p 8443:443") {
		t.Errorf("Plan skipped wildcard = %+v", skipped[0])
	}
	// The remedy must name the machine's publish address as well as the
	// bare "-p": "-p 7070:80" on its own publishes on every interface, so
	// advising it alone answers "keep this off the LAN" with "put it on
	// the LAN".
	if skipped[1].Local != "127.0.0.1:7070" || skipped[1].Remote != "" ||
		!strings.Contains(skipped[1].Error, "guest's own loopback") ||
		!strings.Contains(skipped[1].Error, "-p 7070:80") ||
		!strings.Contains(skipped[1].Error, "--publish-addr 127.0.0.1") {
		t.Errorf("Plan skipped loopback = %+v", skipped[1])
	}
	// Empty and "[]" outputs are empty sets; garbage is an error.
	for _, in := range []string{"", "[]", "null"} {
		if got, err := Desired([]byte(in), guest, ""); err != nil || len(got) != 0 {
			t.Errorf("Desired(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := Desired([]byte("{not json"), guest, ""); err == nil {
		t.Error("garbage accepted")
	}
}

// fakeProvider records Expose/Unexpose calls and serves List from its own
// table; failPorts makes Expose fail for those local addresses.
type fakeProvider struct {
	mu        sync.Mutex
	live      map[string]netprov.Mapping
	exposed   []netprov.Mapping
	unexposed []netprov.Mapping
	failLocal map[string]string
	listErr   error
}

func newFake(seed ...netprov.Mapping) *fakeProvider {
	f := &fakeProvider{live: map[string]netprov.Mapping{}, failLocal: map[string]string{}}
	for _, m := range seed {
		f.live[key(m)] = m
	}
	return f
}

func (f *fakeProvider) Name() string                   { return "fake" }
func (f *fakeProvider) Preflight() error               { return nil }
func (f *fakeProvider) Logs(*machine.Machine) []string { return nil }
func (f *fakeProvider) Capabilities() netprov.Capabilities {
	return netprov.Capabilities{Supervised: true}
}
func (f *fakeProvider) Start(context.Context, *machine.Machine) (backend.NetAttachment, netprov.Endpoint, error) {
	return backend.NetAttachment{}, netprov.Endpoint{GuestIP: guest}, nil
}
func (f *fakeProvider) Stop(context.Context, *machine.Machine) error { return nil }
func (f *fakeProvider) State(*machine.Machine) (backend.State, error) {
	return backend.Running, nil
}
func (f *fakeProvider) Endpoint(*machine.Machine) (netprov.Endpoint, error) {
	return netprov.Endpoint{GuestIP: guest}, nil
}
func (f *fakeProvider) Expose(_ context.Context, _ *machine.Machine, mp netprov.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposed = append(f.exposed, mp)
	if msg, ok := f.failLocal[mp.Local]; ok {
		return errors.New(msg)
	}
	if _, dup := f.live[key(mp)]; dup {
		return errors.New("listen tcp " + mp.Local + ": address already in use")
	}
	f.live[key(mp)] = mp
	return nil
}
func (f *fakeProvider) Unexpose(_ context.Context, _ *machine.Machine, mp netprov.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unexposed = append(f.unexposed, mp)
	delete(f.live, key(mp))
	return nil
}
func (f *fakeProvider) List(context.Context, *machine.Machine) ([]netprov.Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]netprov.Mapping, 0, len(f.live))
	for _, m := range f.live {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeProvider) calls() (exposed, unexposed []netprov.Mapping) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netprov.Mapping(nil), f.exposed...), append([]netprov.Mapping(nil), f.unexposed...)
}

func mapping(proto, local string) netprov.Mapping {
	_, port, _ := strings.Cut(local, ":")
	return netprov.Mapping{Proto: proto, Local: local, Remote: guest + ":" + port}
}

func TestConvergeOwnsOnlyItsOwnMappings(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	// A mapping that was there before us (gvproxy's ssh-port style) must
	// survive untouched, even when it is also desired.
	ssh := netprov.Mapping{Proto: "tcp", Local: "127.0.0.1:2222", Remote: guest + ":22"}
	external := mapping("tcp", "127.0.0.1:9000")
	p := newFake(ssh, external)
	st := &State{}

	web, db := mapping("tcp", "127.0.0.1:8080"), mapping("tcp", "127.0.0.1:5432")
	res, err := Converge(ctx, p, m, []netprov.Mapping{web, db, external}, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Exposed) != 2 || len(res.External) != 1 || len(res.Unexposed) != 0 {
		t.Errorf("first converge: %s", res)
	}
	if len(st.Owned) != 2 || len(st.Errors()) != 0 {
		t.Errorf("owned = %+v", st.Owned)
	}

	// The container on 5432 goes away; 9000 (not ours) stays desired; no
	// change for 8080.
	res, err = Converge(ctx, p, m, []netprov.Mapping{web, external}, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unexposed) != 1 || res.Unexposed[0] != db || len(res.Exposed) != 0 {
		t.Errorf("second converge: %s", res)
	}
	// Everything goes away: only 8080 is ours to remove.
	res, err = Converge(ctx, p, m, nil, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unexposed) != 1 || res.Unexposed[0] != web || len(st.Owned) != 0 {
		t.Errorf("third converge: %s; owned %+v", res, st.Owned)
	}
	_, unexposed := p.calls()
	for _, u := range unexposed {
		if u == ssh || u == external {
			t.Errorf("unexposed a mapping we do not own: %v", u)
		}
	}
	live, _ := p.List(ctx, m)
	if len(live) != 2 {
		t.Errorf("live after converge = %v, want ssh and external only", live)
	}
}

func TestConvergeRecordsAndRetriesErrors(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	p := newFake()
	p.failLocal["127.0.0.1:8080"] = "listen tcp 127.0.0.1:8080: bind: address already in use"
	web := mapping("tcp", "127.0.0.1:8080")
	st := &State{}

	res, err := Converge(ctx, p, m, []netprov.Mapping{web}, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || len(st.Owned) != 1 || !strings.Contains(st.Owned[0].Error, "address already in use") {
		t.Fatalf("res %s; owned %+v", res, st.Owned)
	}
	// The error is visible on disk for "jm ports".
	on, err := Load(path)
	if err != nil || len(on.Errors()) != 1 || on.Owned[0].Status() != "error: "+st.Owned[0].Error {
		t.Errorf("persisted %+v, %v", on, err)
	}
	// The port frees up: the next resync retries and clears the error.
	delete(p.failLocal, "127.0.0.1:8080")
	res, err = Converge(ctx, p, m, []netprov.Mapping{web}, st, path)
	if err != nil || len(res.Exposed) != 1 || len(st.Errors()) != 0 {
		t.Errorf("retry: %s, %v; owned %+v", res, err, st.Owned)
	}
	exposed, _ := p.calls()
	if len(exposed) != 2 {
		t.Errorf("expose called %d times, want 2", len(exposed))
	}
}

func TestConvergeUnsupportedProvider(t *testing.T) {
	p := newFake()
	p.listErr = netprov.ErrUnsupported
	_, err := Converge(context.Background(), p, &machine.Machine{}, nil, &State{}, filepath.Join(t.TempDir(), StateFile))
	if !errors.Is(err, netprov.ErrUnsupported) {
		t.Errorf("err = %v", err)
	}
}

// A restarted forwarder loads the owned set, re-exposes what a restarted
// provider lost, and still never touches foreign mappings.
func TestOwnershipSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	ssh := netprov.Mapping{Proto: "tcp", Local: "127.0.0.1:2222", Remote: guest + ":22"}
	web := mapping("tcp", "127.0.0.1:8080")

	p := newFake(ssh)
	st := &State{}
	if _, err := Converge(ctx, p, m, []netprov.Mapping{web}, st, path); err != nil {
		t.Fatal(err)
	}

	// Restart: new provider instance (table wiped but ssh re-added by its
	// own startup), fresh state loaded from disk.
	p2 := newFake(ssh)
	st2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Owned) != 1 || st2.Owned[0].Mapping() != web {
		t.Fatalf("loaded owned = %+v", st2.Owned)
	}
	res, err := Converge(ctx, p2, m, []netprov.Mapping{web}, st2, path)
	if err != nil || len(res.Exposed) != 1 {
		t.Errorf("re-expose after restart: %s, %v", res, err)
	}
	// Container gone while we were down: owned entry is removed, ssh stays.
	res, err = Converge(ctx, p2, m, nil, st2, path)
	if err != nil || len(res.Unexposed) != 1 || len(st2.Owned) != 0 {
		t.Errorf("cleanup after restart: %s, %v; owned %+v", res, err, st2.Owned)
	}
	_, unexposed := p2.calls()
	if len(unexposed) != 1 || unexposed[0] != web {
		t.Errorf("unexposed %v, want only %v", unexposed, web)
	}
	st3, _ := Load(path)
	if len(st3.Owned) != 0 {
		t.Errorf("disk still owns %+v", st3.Owned)
	}
}

func TestRelease(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	web := mapping("tcp", "127.0.0.1:8080")
	p := newFake()
	st := &State{}
	if _, err := Converge(ctx, p, m, []netprov.Mapping{web}, st, path); err != nil {
		t.Fatal(err)
	}
	if err := Release(ctx, p, m, path); err != nil {
		t.Fatal(err)
	}
	if live, _ := p.List(ctx, m); len(live) != 0 {
		t.Errorf("live after release = %v", live)
	}
	if st, _ := Load(path); len(st.Owned) != 0 {
		t.Errorf("owned after release = %+v", st.Owned)
	}
	// Releasing with no state file is fine.
	if err := Release(ctx, p, m, filepath.Join(t.TempDir(), StateFile)); err != nil {
		t.Error(err)
	}
}

func TestDebounce(t *testing.T) {
	d := newDebouncer(50 * time.Millisecond)
	defer d.stop()
	for i := 0; i < 10; i++ {
		d.Trigger()
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-d.C:
		t.Fatal("fired before the quiet period")
	default:
	}
	select {
	case <-d.C:
	case <-time.After(time.Second):
		t.Fatal("never fired")
	}
	select {
	case <-d.C:
		t.Fatal("fired twice for one burst")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRelevant(t *testing.T) {
	cases := map[string]bool{
		`{"Name":"web","Status":"start","Type":"container","Time":"2026-08-20T10:00:00Z"}`: true,
		`{"Name":"web","Status":"died","Type":"container"}`:                                true,
		`{"Name":"web","Status":"remove","Type":"container"}`:                              true,
		`{"Name":"web","Status":"create","Type":"container"}`:                              false,
		`{"Name":"web","Status":"exec","Type":"container"}`:                                false,
		`{"Name":"nginx","Status":"pull","Type":"image"}`:                                  false,
		`not json`: false,
	}
	for line, want := range cases {
		if got := Relevant([]byte(line)); got != want {
			t.Errorf("Relevant(%s) = %v, want %v", line, got, want)
		}
	}
}

// fakeEngine serves a scripted ps output and an event stream fed through a
// pipe.
type fakeEngine struct {
	mu     sync.Mutex
	ps     []byte
	events *io.PipeReader
	w      *io.PipeWriter
	opened int
}

func (e *fakeEngine) PS(context.Context) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ps, nil
}

func (e *fakeEngine) setPS(b []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ps = b
}

func (e *fakeEngine) Events(context.Context) (io.ReadCloser, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opened++
	if e.opened > 1 {
		// Second connection: block until the test ends.
		r, _ := io.Pipe()
		return r, nil
	}
	return e.events, nil
}

func TestRunReactsToEvents(t *testing.T) {
	r, w := io.Pipe()
	eng := &fakeEngine{ps: []byte("[]"), events: r, w: w}
	p := newFake()
	path := filepath.Join(t.TempDir(), StateFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logBuf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Provider: p, Machine: &machine.Machine{Name: "t"}, GuestIP: guest, Engine: eng,
			StatePath: path, Log: log.New(&logBuf, "", 0),
			Resync: time.Hour, Debounce: 20 * time.Millisecond, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		})
	}()

	// A container starts: ps now lists it and the event arrives.
	eng.setPS([]byte(psJSON))
	if _, err := io.WriteString(w, `{"Name":"web","Status":"start","Type":"container"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { exposed, _ := p.calls(); return len(exposed) == 5 })

	// Stream drops: the forwarder resyncs (container gone) and reconnects.
	eng.setPS([]byte("[]"))
	_ = w.Close()
	waitFor(t, func() bool { _, un := p.calls(); return len(un) == 5 })

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	if !strings.Contains(logBuf.String(), "reconnecting") {
		t.Errorf("log lacks a reconnect line:\n%s", logBuf.String())
	}
}

func TestRunNeedsGuestIP(t *testing.T) {
	err := Run(context.Background(), Config{Provider: newFake(), Engine: &fakeEngine{}, StatePath: filepath.Join(t.TempDir(), StateFile)})
	if err == nil {
		t.Error("Run without a guest address should fail")
	}
}

func TestIsOurs(t *testing.T) {
	p := Process{Dir: "/r/machines/dev", Name: "dev", Root: "/r"}
	if !isOurs("/usr/local/bin/jm --state-root /r _forwarder dev", p) {
		t.Error("own argv not recognised")
	}
	if isOurs("/usr/local/bin/jm --state-root /r _forwarder other", p) {
		t.Error("another machine's forwarder recognised")
	}
	if isOurs("/usr/local/bin/jm --state-root /other _forwarder dev", p) {
		t.Error("another state root's forwarder recognised")
	}
	if isOurs("qemu-system-aarch64 -name dev", p) {
		t.Error("unrelated process recognised")
	}
	sp := Process{Dir: "/Users/me/My State/machines/dev", Name: "dev", Root: "/Users/me/My State"}
	if !isOurs("/usr/local/bin/jm --state-root /Users/me/My State _forwarder dev", sp) {
		t.Error("state root with a space not recognised")
	}
	if isOurs("/usr/local/bin/jm --state-root /Users/me/My State _forwarder devel", sp) {
		t.Error("name prefix match recognised")
	}
	// A stale pid file pointing at a pid that cannot exist is not alive.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PIDFile), []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := Process{Dir: dir, Name: "dev", Root: "/r"}
	if _, ok := pr.Alive(); ok {
		t.Error("stale pid reported alive")
	}
	if err := pr.Stop(context.Background()); err != nil {
		t.Errorf("Stop on stale pid: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Error("stale pid file not removed")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestReleaseSkipsMappingsTheProviderDoesNotList(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	web := mapping("tcp", "127.0.0.1:8080")
	gone := mapping("tcp", "127.0.0.1:9090")
	p := newFake(web)
	st := &State{Owned: []Entry{
		{Proto: web.Proto, Local: web.Local, Remote: web.Remote},
		{Proto: gone.Proto, Local: gone.Local, Remote: gone.Remote, Error: "address already in use"},
		{Proto: "tcp", Local: "0.0.0.0:5353", Error: "unpublishable"},
	}}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := Release(ctx, p, m, path); err != nil {
		t.Fatal(err)
	}
	if _, unexposed := p.calls(); !reflect.DeepEqual(unexposed, []netprov.Mapping{web}) {
		t.Errorf("unexposed = %v, want only %v", unexposed, web)
	}
	if st, _ := Load(path); len(st.Owned) != 0 {
		t.Errorf("owned after release = %+v", st.Owned)
	}
}

func TestConvergeDropsStaleUnpublishableSilently(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	p := newFake()
	st := &State{Owned: []Entry{{Proto: "tcp", Local: "0.0.0.0:5353", Error: "unpublishable"}}}
	res, err := Converge(ctx, p, m, nil, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unexposed) != 0 || len(res.Failed) != 0 || res.String() != "no change" {
		t.Errorf("result = %q, want no change", res)
	}
	if _, unexposed := p.calls(); len(unexposed) != 0 {
		t.Errorf("unexposed = %v, want none", unexposed)
	}
	if len(st.Owned) != 0 {
		t.Errorf("owned = %+v, want empty", st.Owned)
	}
}

func TestLeaked(t *testing.T) {
	ssh := netprov.Mapping{Proto: "tcp", Local: "127.0.0.1:50022", Remote: guest + ":22"}
	web := mapping("tcp", "127.0.0.1:8080")
	other := netprov.Mapping{Proto: "tcp", Local: "127.0.0.1:1", Remote: "10.0.0.9:1"}
	got := Leaked([]netprov.Mapping{ssh, web, other}, guest, "127.0.0.1:50022")
	if !reflect.DeepEqual(got, []netprov.Mapping{web}) {
		t.Errorf("Leaked = %v, want [%v]", got, web)
	}
	if got := Leaked([]netprov.Mapping{ssh}, guest, ""); len(got) != 0 {
		t.Errorf("Leaked without sshLocal = %v, want none", got)
	}
}

func TestRunLogsLeakedMappings(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	eng := &fakeEngine{ps: []byte("[]"), events: r, w: w}
	web := mapping("tcp", "127.0.0.1:8080")
	p := newFake(web)
	ctx, cancel := context.WithCancel(context.Background())
	var logBuf bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Provider: p, Machine: &machine.Machine{Name: "t"}, GuestIP: guest, Engine: eng,
			StatePath: filepath.Join(t.TempDir(), StateFile), Log: log.New(&logBuf, "", 0),
			Resync: time.Hour, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "jm stop && jm start resets them") || !strings.Contains(logBuf.String(), web.String()) {
		t.Errorf("log = %q, want a leaked-mapping hint naming %v", logBuf.String(), web)
	}
	if _, unexposed := p.calls(); len(unexposed) != 0 {
		t.Errorf("adopted and unexposed %v", unexposed)
	}
}

// A port published in the guest but unreachable from the host is reported
// once, when the forwarder first sees it, so it appears in the log rather
// than only in "jm ports" — which nobody runs before assuming a published
// port works. Later resyncs say nothing: it is not news any more.
func TestConvergeReportsUnpublishableOnce(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	p := newFake()
	st := &State{}
	skipped := []Entry{{Proto: "tcp", Local: "127.0.0.1:8091", Error: "binds the guest's own loopback"}}

	res, err := ConvergeWith(ctx, p, m, nil, skipped, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Local != "127.0.0.1:8091" {
		t.Fatalf("first sight: skipped = %+v", res.Skipped)
	}
	if !res.changed() || !strings.Contains(res.String(), "warning: tcp 127.0.0.1:8091") {
		t.Errorf("first sight: %q", res)
	}

	res, err = ConvergeWith(ctx, p, m, nil, skipped, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 0 || res.String() != "no change" {
		t.Errorf("second sight: %q", res)
	}
	if exposed, unexposed := p.calls(); len(exposed) != 0 || len(unexposed) != 0 {
		t.Errorf("the provider was touched: %v %v", exposed, unexposed)
	}
}
