package forwarder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	// A publish that names no host address binds every host interface, as
	// docker does on Linux. One that names a host address binds that
	// address and only that one — again as docker does — which is why
	// 0.0.0.0:8443 and 127.0.0.1:7070 are here rather than in an apology.
	// The range becomes one mapping per port, udp keeps its protocol, and
	// the duplicate host port is published once.
	want := []netprov.Mapping{
		{Proto: "tcp", Local: "0.0.0.0:7071", Remote: guest + ":7071"},
		{Proto: "tcp", Local: "0.0.0.0:8080", Remote: guest + ":8080"},
		{Proto: "tcp", Local: "0.0.0.0:8443", Remote: guest + ":8443"},
		{Proto: "tcp", Local: "127.0.0.1:7070", Remote: guest + ":7070"},
		{Proto: "udp", Local: "0.0.0.0:6000", Remote: guest + ":6000"},
		{Proto: "udp", Local: "0.0.0.0:6001", Remote: guest + ":6001"},
		{Proto: "udp", Local: "0.0.0.0:6002", Remote: guest + ":6002"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Desired =\n%v\nwant\n%v", got, want)
	}
	// The two that name a host address are the two the engine leaves
	// unreachable inside the guest, so jm redirects them itself; the rest
	// need nothing.
	pl, err := Compute([]byte(psJSON), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Unpublishable) != 0 {
		t.Errorf("Unpublishable = %+v, want none", pl.Unpublishable)
	}
	wantRules := map[int]string{8443: "a1", 7070: "f6"}
	if len(pl.Rules) != len(wantRules) {
		t.Fatalf("rules = %+v", pl.Rules)
	}
	for _, r := range pl.Rules {
		if id, ok := wantRules[r.GuestPort]; !ok || r.ContainerID != id || r.GuestIP != guest {
			t.Errorf("rule %+v is not one of %v", r, wantRules)
		}
	}
	// A rule is only worth loading once its container's address is known.
	if AnchorText(pl.Rules) != "" {
		t.Error("an unresolved rule reached the anchor")
	}
	pl.Resolve(map[string]string{"a1": "10.88.0.4", "f6": "10.88.0.7"})
	if got, want := AnchorText(pl.Rules),
		"rdr pass inet proto tcp from any to "+guest+" port = 7070 -> 10.88.0.7 port 80\n"+
			"rdr pass inet proto tcp from any to "+guest+" port = 8443 -> 10.88.0.4 port 443\n"; got != want {
		t.Errorf("AnchorText =\n%q\nwant\n%q", got, want)
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
	mu        sync.Mutex
	ps        []byte
	ips       map[string]string
	inspected []string
	events    *io.PipeReader
	w         *io.PipeWriter
	opened    int
	listed    int
}

func (e *fakeEngine) PS(context.Context) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.listed++
	return e.ps, nil
}

// reconciles counts the resyncs so far: every one lists the containers.
func (e *fakeEngine) reconciles() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listed
}

// Inspect answers with the addresses the test gave it, in the shape podman
// prints.
func (e *fakeEngine) Inspect(_ context.Context, ids []string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inspected = append(e.inspected, ids...)
	var b strings.Builder
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"Id":%q,"NetworkSettings":{"Networks":{"podman":{"IPAddress":%q}}}}`, id, e.ips[id])
	}
	b.WriteByte(']')
	return []byte(b.String()), nil
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
	waitFor(t, func() bool { exposed, _ := p.calls(); return len(exposed) == 7 })

	// Stream drops: the forwarder resyncs (container gone) and reconnects.
	eng.setPS([]byte("[]"))
	_ = w.Close()
	waitFor(t, func() bool { _, un := p.calls(); return len(un) == 7 })

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

	res, err := ConvergeWith(ctx, p, m, Plan{Unpublishable: skipped}, st, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Local != "127.0.0.1:8091" {
		t.Fatalf("first sight: skipped = %+v", res.Skipped)
	}
	if !res.changed() || !strings.Contains(res.String(), "warning: tcp 127.0.0.1:8091") {
		t.Errorf("first sight: %q", res)
	}

	res, err = ConvergeWith(ctx, p, m, Plan{Unpublishable: skipped}, st, path)
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

// fakeGuest records every rule set loaded into the guest's anchor and
// keeps the last one as the anchor's content, so that a test can drop it
// behind the forwarder's back the way a guest reboot, "service pf restart"
// or "pfctl -F nat" does.
type fakeGuest struct {
	mu      sync.Mutex
	loaded  []string
	current string
	err     error
}

func (g *fakeGuest) ApplyRules(_ context.Context, text string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	g.loaded = append(g.loaded, text)
	g.current = text
	return nil
}

// anchor is what the guest holds now.
func (g *fakeGuest) anchor() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current
}

// drop empties the anchor without telling the forwarder, as the guest
// itself can at any moment.
func (g *fakeGuest) drop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current = ""
}

func (g *fakeGuest) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.loaded...)
}

// The guest-side half of publishing: a "-p 127.0.0.1:8080:80" is bound on
// the host's loopback and made reachable inside the guest by a rule jm
// loads itself, because the engine bound the guest's loopback instead. The
// anchor is loaded whole and only when it changes, so an idle machine costs
// no SSH round trips, and a machine with nothing published has its anchor
// cleared once at startup rather than left with a dead forwarder's rules.
func TestRunLoadsGuestRedirects(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	eng := &fakeEngine{ps: []byte("[]"), events: r, w: w,
		ips: map[string]string{"a1": "10.88.0.4", "f6": "10.88.0.7"}}
	g := &fakeGuest{}
	p := newFake()
	path := filepath.Join(t.TempDir(), StateFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Provider: p, Machine: &machine.Machine{Name: "t"}, GuestIP: guest, HostIP: "127.0.0.1",
			Engine: eng, Guest: g, StatePath: path, Log: log.New(io.Discard, "", 0),
			Resync: 20 * time.Millisecond, Debounce: time.Millisecond,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		})
	}()
	// Nothing published: the anchor is cleared once, then left alone.
	waitFor(t, func() bool { return len(g.calls()) == 1 })
	if got := g.calls()[0]; got != "" {
		t.Errorf("first load = %q, want the anchor flushed", got)
	}

	eng.setPS([]byte(psJSON))
	waitFor(t, func() bool {
		for _, text := range g.calls() {
			if strings.Contains(text, "port = 7070 -> 10.88.0.7 port 80") {
				return true
			}
		}
		return false
	})
	// Only the containers that need a redirect are inspected.
	eng.mu.Lock()
	inspected := append([]string(nil), eng.inspected...)
	eng.mu.Unlock()
	for _, id := range inspected {
		if id != "a1" && id != "f6" {
			t.Errorf("inspected %q, which publishes nothing that needs a redirect", id)
		}
	}
	// A steady state does not rewrite the anchor on every reconcile: the
	// memo spares the SSH round trip. It is not "never" — every
	// ReloadEvery-th timer resync writes it again on purpose, because the
	// memo is a belief about a guest that can lose its pf state without
	// telling anyone (TestRunReloadsGuestAnchorAfterTheGuestLosesIt).
	before, reconciled := len(g.calls()), eng.reconciles()
	time.Sleep(120 * time.Millisecond)
	rewrites, reconciles := len(g.calls())-before, eng.reconciles()-reconciled
	if rewrites*2 > reconciles {
		t.Errorf("anchor rewritten %d times in %d reconciles with nothing changed; the memo is not sparing the round trip", rewrites, reconciles)
	}
	// The redirected mappings are ok, not errors: both halves are in place.
	on, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(on.Errors()) != 0 {
		t.Errorf("errors = %+v, want none", on.Errors())
	}
	if on.PublishAddr != "127.0.0.1" {
		t.Errorf("state publish_addr = %q, want the address this forwarder binds", on.PublishAddr)
	}
	// A publish that names a host address binds it, not the machine's
	// default: "-p 127.0.0.1:7070:80" on a machine publishing on
	// 127.0.0.1 is still the loopback, and -p 0.0.0.0:8443:443 is not.
	var seen []string
	for _, e := range on.Owned {
		seen = append(seen, e.Local)
	}
	sort.Strings(seen)
	want := []string{"0.0.0.0:8443", "127.0.0.1:6000", "127.0.0.1:6001", "127.0.0.1:6002",
		"127.0.0.1:7070", "127.0.0.1:7071", "127.0.0.1:8080"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("owned locals = %v, want %v", seen, want)
	}

	// The containers go away: the anchor is emptied again.
	eng.setPS([]byte("[]"))
	waitFor(t, func() bool {
		c := g.calls()
		return len(c) > 1 && c[len(c)-1] == ""
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The rule set the forwarder last loaded is a belief about the guest, not
// a reading of it: the guest can lose its pf state on its own (a reboot
// with restart-policy containers coming back on the same addresses,
// "service pf restart", "pfctl -F nat") without anything in the desired
// state changing. Skipping the reload because the plan is unchanged would
// leave every host-bound publish bound on the host and answering nothing,
// while "jm ports" still said ok. So the loop reloads the anchor
// unconditionally every ReloadEvery timer resyncs, whatever the memo says.
func TestRunReloadsGuestAnchorAfterTheGuestLosesIt(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	eng := &fakeEngine{ps: []byte(psJSON), events: r, w: w,
		ips: map[string]string{"a1": "10.88.0.4", "f6": "10.88.0.7"}}
	g := &fakeGuest{}
	p := newFake()
	path := filepath.Join(t.TempDir(), StateFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Provider: p, Machine: &machine.Machine{Name: "t"}, GuestIP: guest,
			Engine: eng, Guest: g, StatePath: path, Log: log.New(io.Discard, "", 0),
			Resync: 20 * time.Millisecond, Debounce: time.Millisecond,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
			ReloadEvery: 1,
		})
	}()
	const rule = "port = 7070 -> 10.88.0.7 port 80"
	waitFor(t, func() bool { return strings.Contains(g.anchor(), rule) })

	// The guest flushes the anchor behind the forwarder's back.
	g.drop()
	waitFor(t, func() bool { return strings.Contains(g.anchor(), rule) })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A dropped event stream is a gap in what jm knows about the guest, which
// may have rebooted while it was blind: the resync that follows the
// reconnect reloads the anchor whether or not the rules changed (ADR 0004:
// a full re-sync on start, on reconnect and on a timer). The timer is out
// of reach here (an hour), so only the reconnect can heal the anchor.
func TestEventStreamReconnectReloadsGuestAnchor(t *testing.T) {
	r, w := io.Pipe()
	eng := &fakeEngine{ps: []byte(psJSON), events: r, w: w,
		ips: map[string]string{"a1": "10.88.0.4", "f6": "10.88.0.7"}}
	g := &fakeGuest{}
	p := newFake()
	path := filepath.Join(t.TempDir(), StateFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Provider: p, Machine: &machine.Machine{Name: "t"}, GuestIP: guest,
			Engine: eng, Guest: g, StatePath: path, Log: log.New(io.Discard, "", 0),
			Resync: time.Hour, Debounce: time.Millisecond,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
			ReloadEvery: 1 << 20,
		})
	}()
	const rule = "port = 7070 -> 10.88.0.7 port 80"
	waitFor(t, func() bool { return strings.Contains(g.anchor(), rule) })

	// The guest loses its rules and the stream drops: reconnecting is the
	// forwarder's only chance to notice, and it takes it.
	g.drop()
	_ = w.Close()
	waitFor(t, func() bool { return strings.Contains(g.anchor(), rule) })

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A guest-side failure is per mapping and visible in "jm ports": the host
// leg is bound (docker binds it too) but the mapping says why nothing
// answers yet, and the next reconcile retries.
func TestGuestRedirectFailureIsPerMapping(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	eng := &fakeEngine{ps: []byte(psJSON), events: r, w: w, ips: map[string]string{"a1": "10.88.0.4", "f6": "10.88.0.7"}}
	g := &fakeGuest{err: errors.New("pfctl: /dev/pf: Permission denied")}
	p := newFake()
	path := filepath.Join(t.TempDir(), StateFile)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Provider: p, Machine: &machine.Machine{Name: "t"}, GuestIP: guest, Engine: eng, Guest: g,
			StatePath: path, Log: log.New(io.Discard, "", 0),
			Resync: 20 * time.Millisecond, MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		})
	}()
	waitFor(t, func() bool {
		st, err := Load(path)
		return err == nil && len(st.Errors()) == 2
	})
	st, _ := Load(path)
	for _, e := range st.Errors() {
		if e.Local != "0.0.0.0:8443" && e.Local != "127.0.0.1:7070" {
			t.Errorf("%s should not depend on a guest redirect", e.Local)
		}
		if !strings.Contains(e.Error, "Permission denied") {
			t.Errorf("%s: %q, want the guest's own words", e.Local, e.Error)
		}
	}
	// The host leg is bound all the same.
	exposed, _ := p.calls()
	if len(exposed) != 7 {
		t.Errorf("exposed %d mappings, want all 7", len(exposed))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Status updates land on the entries that are actually saved, even when the
// same reconcile adds a mapping. Recording a new owned entry used to append
// to st.Owned mid-loop, which reallocates its backing array and orphans the
// pointers the index handed out, so the cleared errors below went nowhere:
// a port that had just come up went on reading "error: ..." in "jm ports"
// until a later reconcile happened to append nothing.
func TestConvergeClearsErrorsWhileAddingAMapping(t *testing.T) {
	ctx := context.Background()
	m := &machine.Machine{Name: "t"}
	path := filepath.Join(t.TempDir(), StateFile)
	live := []netprov.Mapping{
		{Proto: "tcp", Local: "0.0.0.0:8081", Remote: guest + ":8081"},
		{Proto: "tcp", Local: "0.0.0.0:8082", Remote: guest + ":8082"},
	}
	p := newFake(live...)
	// Two owned, live mappings whose guest-side leg was pending last time,
	// stored in a slice with no spare capacity so the append reallocates.
	st := &State{Owned: []Entry{
		{Proto: "tcp", Local: "0.0.0.0:8081", Remote: guest + ":8081", Error: "no address yet"},
		{Proto: "tcp", Local: "0.0.0.0:8082", Remote: guest + ":8082", Error: "no address yet"},
	}}
	// The new mapping sorts before both, so it is appended first.
	fresh := netprov.Mapping{Proto: "tcp", Local: "0.0.0.0:8080", Remote: guest + ":8080"}
	if _, err := ConvergeWith(ctx, p, m, Plan{Mappings: append([]netprov.Mapping{fresh}, live...)}, st, path); err != nil {
		t.Fatal(err)
	}
	for _, got := range []*State{st, mustLoad(t, path)} {
		if len(got.Owned) != 3 {
			t.Fatalf("owned = %+v, want three mappings", got.Owned)
		}
		if errs := got.Errors(); len(errs) != 0 {
			t.Errorf("errors = %+v, want none: the pending reasons are stale", errs)
		}
	}
}

func mustLoad(t *testing.T, path string) *State {
	t.Helper()
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
