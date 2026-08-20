package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/image"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/netprov/gvproxy"
)

// run executes the root command with args against an isolated state root
// and returns stdout and the error.
func run(t *testing.T, root string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	oldOut := stdout
	stdout = &buf
	defer func() { stdout = oldOut }()
	cmd := NewRootCmd()
	cmd.SetArgs(append([]string{"--state-root", root}, args...))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	err := cmd.Execute()
	return buf.String(), err
}

func seedRecord(t *testing.T, root, name string) *machine.Machine {
	t.Helper()
	m := machine.Defaults()
	m.Name = name
	m.Backend = backend.DefaultForHost()
	m.Created = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if err := machine.NewStore(root).Save(&m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestListEmpty(t *testing.T) {
	out, err := run(t, t.TempDir(), "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "NAME") || strings.Count(out, "\n") != 1 {
		t.Errorf("unexpected output:\n%s", out)
	}
	out, err = run(t, t.TempDir(), "--json", "list")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json list = %q, %v", out, err)
	}
}

func TestListAndInspectStopped(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "alpha")
	seedRecord(t, root, "beta")

	out, err := run(t, root, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "alpha") || !strings.Contains(lines[1], "stopped") || !strings.Contains(lines[1], "127.0.0.1:2222") {
		t.Errorf("unexpected list:\n%s", out)
	}

	out, err = run(t, root, "--json", "inspect", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var i struct {
		Name    string `json:"name"`
		State   string `json:"state"`
		SSH     string `json:"ssh"`
		Console string `json:"console"`
		Dir     string `json:"dir"`
	}
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	wantDir := filepath.Join(root, "machines", "alpha")
	if i.Name != "alpha" || i.State != "stopped" || i.SSH != "127.0.0.1:2222" || i.Dir != wantDir || i.Console != filepath.Join(wantDir, "console.log") {
		t.Errorf("unexpected inspect: %+v", i)
	}

	out, err = run(t, root, "inspect", "alpha")
	if err != nil || !strings.Contains(out, "State:") || !strings.Contains(out, "stopped") {
		t.Errorf("text inspect = %q, %v", out, err)
	}
}

func TestInspectMissing(t *testing.T) {
	_, err := run(t, t.TempDir(), "inspect")
	if err == nil || !strings.Contains(err.Error(), "jm init") {
		t.Errorf("err = %v", err)
	}
	_, err = run(t, t.TempDir(), "inspect", "Bad_Name")
	if err == nil || !strings.Contains(err.Error(), "invalid machine name") {
		t.Errorf("err = %v", err)
	}
}

func TestStartAndStopMissing(t *testing.T) {
	for _, c := range []string{"start", "stop", "ssh"} {
		if _, err := run(t, t.TempDir(), c); err == nil {
			t.Errorf("%s on a missing machine should fail", c)
		}
	}
}

func TestStopStoppedIsNoop(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "jailmachine")
	out, err := run(t, root, "stop")
	if err != nil || !strings.Contains(out, "not running") {
		t.Errorf("stop = %q, %v", out, err)
	}
}

func TestRmConverges(t *testing.T) {
	root := t.TempDir()
	out, err := run(t, root, "rm", "ghost")
	if err != nil || !strings.Contains(out, "nothing to remove") {
		t.Errorf("rm ghost = %q, %v", out, err)
	}
	// Half-initialised directory without a record is removed too.
	dir := filepath.Join(root, "machines", "half")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "rm", "half"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists", dir)
	}
}

// A corrupt machine.json must not stop rm from converging; the default
// backend gets a chance to tidy runtime files first.
func TestRmCorruptRecord(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "machines", "bad")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, machine.RecordFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stale pid file from a dead hypervisor must be repaired, not fatal.
	if err := os.WriteFile(filepath.Join(dir, "qemu.pid"), []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "rm", "bad")
	if err != nil || !strings.Contains(out, "removed") {
		t.Fatalf("rm = %q, %v", out, err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("%s still exists", dir)
	}
}

func TestStateRootIsAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := run(t, "rel", "list"); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(StateRoot()) {
		t.Errorf("state root %q not absolute", StateRoot())
	}
}

func TestImageSourceResolvesRelease(t *testing.T) {
	_, ref, err := imageSource(machine.ImageRef{Source: "official"}, 64)
	if err != nil || ref.String() != "official:"+image.DefaultRelease {
		t.Errorf("ref = %q, %v", ref, err)
	}
	_, ref, err = imageSource(machine.ImageRef{Source: "official", Release: "14.3-RELEASE"}, 64)
	if err != nil || ref.String() != "official:14.3-RELEASE" {
		t.Errorf("ref = %q, %v", ref, err)
	}
	if _, _, err := imageSource(machine.ImageRef{Source: "nope"}, 64); err == nil {
		t.Error("unknown source accepted")
	}
}

func TestInitRejectsBadFlags(t *testing.T) {
	root := t.TempDir()
	cases := [][]string{
		{"init", "--cpus", "0"},
		{"init", "--memory", "64"},
		{"init", "--disk", "0"},
		{"init", "--ssh-port", "70000"},
		{"init", "--image", "nope:1"},
		{"init", "--image", ":1"},
		{"init", "Bad"},
	}
	for _, args := range cases {
		if _, err := run(t, root, args...); err == nil {
			t.Errorf("%v should fail", args)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "machines")); len(entries) != 0 {
		t.Errorf("validation failures must not create state: %v", entries)
	}
}

func TestInitRefusesExisting(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "jailmachine")
	_, err := run(t, root, "init")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v", err)
	}
}

func TestSplitSSHArgs(t *testing.T) {
	exists := func(n string) bool { return n == "dev" }
	name, rest := splitSSHArgs(nil, exists)
	if name != machine.DefaultName || len(rest) != 0 {
		t.Errorf("nil: %q %v", name, rest)
	}
	name, rest = splitSSHArgs([]string{"dev", "ls", "-la"}, exists)
	if name != "dev" || strings.Join(rest, " ") != "ls -la" {
		t.Errorf("dev: %q %v", name, rest)
	}
	name, rest = splitSSHArgs([]string{"uname", "-a"}, exists)
	if name != machine.DefaultName || strings.Join(rest, " ") != "uname -a" {
		t.Errorf("cmd: %q %v", name, rest)
	}
}

func seedGvproxyRecord(t *testing.T, root, name string) *machine.Machine {
	t.Helper()
	m := seedRecord(t, root, name)
	m.Network = gvproxy.Name
	if err := machine.NewStore(root).Save(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCombineState(t *testing.T) {
	cases := []struct {
		bs, ps     backend.State
		supervised bool
		want       backend.State
	}{
		{backend.Running, backend.Running, true, backend.Running},
		{backend.Stopped, backend.Stopped, true, backend.Stopped},
		{backend.Running, backend.Stopped, true, backend.Broken},
		{backend.Stopped, backend.Running, true, backend.Broken},
		{backend.Broken, backend.Running, true, backend.Broken},
		{backend.Running, backend.Broken, true, backend.Broken},
		// An unsupervised provider (slirp) always reports running; it
		// cannot disagree with a stopped hypervisor.
		{backend.Stopped, backend.Running, false, backend.Stopped},
		{backend.Running, backend.Running, false, backend.Running},
	}
	for _, c := range cases {
		if got := combineState(c.bs, c.ps, c.supervised); got != c.want {
			t.Errorf("combine(%s, %s, %v) = %s, want %s", c.bs, c.ps, c.supervised, got, c.want)
		}
	}
}

// A record without a network field is a pre-provider machine on slirp.
func TestLegacyRecordUsesUserProvider(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "old")
	out, err := run(t, root, "--json", "inspect", "old")
	if err != nil {
		t.Fatal(err)
	}
	var i struct {
		State        string `json:"state"`
		NetworkState string `json:"network_state"`
		PodmanSock   string `json:"podman_sock_uri"`
	}
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		t.Fatal(err)
	}
	if i.State != "stopped" || i.NetworkState != "running" || i.PodmanSock != "" {
		t.Errorf("unexpected inspect: %s", out)
	}
	if _, err := run(t, root, "env", "old"); err == nil || !strings.Contains(err.Error(), "no API socket") {
		t.Errorf("env on slirp machine: %v", err)
	}
}

func TestInspectGvproxyShowsSocketConnection(t *testing.T) {
	root := t.TempDir()
	m := seedGvproxyRecord(t, root, "gv")
	sock := gvproxy.PathsFor(m.Dir).Podman

	out, err := run(t, root, "--json", "inspect", "gv")
	if err != nil {
		t.Fatal(err)
	}
	var i struct {
		State      string `json:"state"`
		Network    string `json:"network"`
		PodmanSock string `json:"podman_sock_uri"`
		APISocket  string `json:"api_socket"`
		DNS        string `json:"dns"`
	}
	if err := json.Unmarshal([]byte(out), &i); err != nil {
		t.Fatal(err)
	}
	if i.State != "stopped" || i.Network != "gvproxy" || i.APISocket != sock || i.PodmanSock != "unix://"+sock || i.DNS != gvproxy.Gateway {
		t.Errorf("unexpected inspect: %s", out)
	}
	text, err := run(t, root, "inspect", "gv")
	if err != nil || !strings.Contains(text, "gv-sock") || !strings.Contains(text, "Network:") {
		t.Errorf("text inspect = %q, %v", text, err)
	}
}

func TestEnvOutput(t *testing.T) {
	root := t.TempDir()
	m := seedGvproxyRecord(t, root, "gv")
	uri := "unix://" + gvproxy.PathsFor(m.Dir).Podman

	out, err := run(t, root, "env", "gv")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export CONTAINER_HOST=\"" + uri + "\"\n",
		// podman resolves CONTAINER_CONNECTION before CONTAINER_HOST: it
		// must name the socket connection, not the ssh one.
		"export CONTAINER_CONNECTION=\"gv-sock\"\n",
		"export DOCKER_HOST=\"" + uri + "\"\n",
		"# run: eval \"$(jm env gv)\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("env output missing %q:\n%s", want, out)
		}
	}
	out, err = run(t, root, "env", "gv", "--shell", "fish")
	if err != nil || !strings.Contains(out, "set -gx DOCKER_HOST \""+uri+"\";") || !strings.Contains(out, "eval (jm env gv --shell fish)") {
		t.Errorf("fish env = %q, %v", out, err)
	}
	if _, err := run(t, root, "env", "gv", "--shell", "csh"); err == nil {
		t.Error("unknown shell accepted")
	}
}

// A stale gvproxy pid file with no hypervisor makes the machine broken;
// stop repairs it (removes the pid file) and the machine reads stopped.
func TestStopRepairsStaleProvider(t *testing.T) {
	root := t.TempDir()
	m := seedGvproxyRecord(t, root, "gv")
	pid := gvproxy.PathsFor(m.Dir).PID
	if err := os.WriteFile(pid, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "list")
	if err != nil || !strings.Contains(out, "broken") {
		t.Fatalf("list = %q, %v", out, err)
	}
	out, err = run(t, root, "stop", "gv")
	if err != nil || !strings.Contains(out, "repairing") {
		t.Fatalf("stop = %q, %v", out, err)
	}
	if _, err := os.Stat(pid); !os.IsNotExist(err) {
		t.Errorf("stale pid file survived repair")
	}
	out, err = run(t, root, "list")
	if err != nil || !strings.Contains(out, "stopped") {
		t.Errorf("list after repair = %q, %v", out, err)
	}
}

func TestHelpListsEnv(t *testing.T) {
	out, err := run(t, t.TempDir(), "--help")
	if err != nil || !strings.Contains(out, "\n  env ") {
		t.Errorf("help = %q, %v", out, err)
	}
}

// fakeBackend and fakeProvider let the broken-state repair be exercised
// without a hypervisor: their states are scripted and Stop calls recorded.
type fakeBackend struct {
	state    backend.State
	stops    []bool
	stopArgs *[]bool
}

func (f *fakeBackend) Name() string     { return "fakebe" }
func (f *fakeBackend) Preflight() error { return nil }
func (f *fakeBackend) Start(context.Context, *machine.Machine, backend.NetAttachment) error {
	return nil
}
func (f *fakeBackend) Stop(_ context.Context, _ *machine.Machine, graceful bool) error {
	f.stops = append(f.stops, graceful)
	f.state = backend.Stopped
	return nil
}
func (f *fakeBackend) State(*machine.Machine) (backend.State, error) { return f.state, nil }
func (f *fakeBackend) ConsolePath(*machine.Machine) string           { return "" }
func (f *fakeBackend) Logs(*machine.Machine) []string                { return nil }
func (f *fakeBackend) Capabilities() backend.Capabilities            { return backend.Capabilities{} }

type fakeProvider struct {
	state backend.State
	stops int
}

func (f *fakeProvider) Name() string                   { return "fakenet" }
func (f *fakeProvider) Preflight() error               { return nil }
func (f *fakeProvider) Logs(*machine.Machine) []string { return nil }
func (f *fakeProvider) Capabilities() netprov.Capabilities {
	return netprov.Capabilities{Supervised: true}
}
func (f *fakeProvider) Start(context.Context, *machine.Machine) (backend.NetAttachment, netprov.Endpoint, error) {
	return backend.NetAttachment{}, netprov.Endpoint{}, nil
}
func (f *fakeProvider) Stop(context.Context, *machine.Machine) error {
	f.stops++
	f.state = backend.Stopped
	return nil
}
func (f *fakeProvider) State(*machine.Machine) (backend.State, error) { return f.state, nil }
func (f *fakeProvider) Endpoint(*machine.Machine) (netprov.Endpoint, error) {
	return netprov.Endpoint{SSHHost: machine.SSHHost, SSHPort: 2222}, nil
}
func (f *fakeProvider) Expose(context.Context, *machine.Machine, netprov.Mapping) error {
	return netprov.ErrUnsupported
}
func (f *fakeProvider) Unexpose(context.Context, *machine.Machine, netprov.Mapping) error {
	return netprov.ErrUnsupported
}
func (f *fakeProvider) List(context.Context, *machine.Machine) ([]netprov.Mapping, error) {
	return nil, netprov.ErrUnsupported
}

var (
	fakeBE  = &fakeBackend{}
	fakeNet = &fakeProvider{}
)

func init() {
	backend.Register(fakeBE)
	netprov.Register(fakeNet)
}

func seedFakeRecord(t *testing.T, root, name string) {
	t.Helper()
	m := seedRecord(t, root, name)
	m.Backend = fakeBE.Name()
	m.Network = fakeNet.Name()
	if err := machine.NewStore(root).Save(m); err != nil {
		t.Fatal(err)
	}
}

// A live hypervisor whose network provider died is Broken; repairing it
// must shut the guest down gracefully, not pull the plug. A dead
// hypervisor with a stale provider is torn down forcibly; --force too.
func TestRepairBrokenKeepsGuestShutdownGraceful(t *testing.T) {
	root := t.TempDir()
	seedFakeRecord(t, root, "fk")
	cases := []struct {
		bs, ps   backend.State
		args     []string
		wantStop []bool
	}{
		{backend.Running, backend.Stopped, []string{"stop", "fk"}, []bool{true}},
		{backend.Running, backend.Broken, []string{"stop", "fk"}, []bool{true}},
		{backend.Running, backend.Stopped, []string{"stop", "--force", "fk"}, []bool{false}},
		{backend.Broken, backend.Running, []string{"stop", "fk"}, []bool{false}},
		{backend.Stopped, backend.Running, []string{"stop", "fk"}, []bool{false}},
	}
	for _, c := range cases {
		fakeBE.state, fakeBE.stops = c.bs, nil
		fakeNet.state, fakeNet.stops = c.ps, 0
		out, err := run(t, root, c.args...)
		if err != nil || !strings.Contains(out, "repairing") {
			t.Fatalf("%v (%s/%s) = %q, %v", c.args, c.bs, c.ps, out, err)
		}
		if len(fakeBE.stops) != len(c.wantStop) || fakeBE.stops[0] != c.wantStop[0] {
			t.Errorf("%v (%s/%s): backend Stop graceful = %v, want %v", c.args, c.bs, c.ps, fakeBE.stops, c.wantStop)
		}
		if fakeNet.stops != 1 {
			t.Errorf("%v (%s/%s): provider Stop called %d times", c.args, c.bs, c.ps, fakeNet.stops)
		}
		if out, _ := run(t, root, "list"); !strings.Contains(out, "stopped") {
			t.Errorf("list after repair = %q", out)
		}
	}
}

func TestParseKernelVersions(t *testing.T) {
	cases := []struct {
		in            string
		disk, running string
		ok            bool
	}{
		{"15.1-RELEASE-p2\n15.1-RELEASE\n", "15.1-RELEASE-p2", "15.1-RELEASE", true},
		{"14.3-RELEASE\n14.3-RELEASE\n", "14.3-RELEASE", "14.3-RELEASE", true},
		{"", "", "", false},
		{"15.1-RELEASE\n", "", "", false},
		{"a\nb\nc\n", "", "", false},
	}
	for _, c := range cases {
		disk, running, ok := parseKernelVersions(c.in)
		if disk != c.disk || running != c.running || ok != c.ok {
			t.Errorf("parseKernelVersions(%q) = %q, %q, %v; want %q, %q, %v", c.in, disk, running, ok, c.disk, c.running, c.ok)
		}
	}
}

// The forwarder's persisted table is visible through ports, inspect and
// list without the machine (or the forwarder) running.
func TestPortsInspectAndListShowForwards(t *testing.T) {
	root := t.TempDir()
	m := seedGvproxyRecord(t, root, "gv")
	st := &forwarder.State{Owned: []forwarder.Entry{
		{Proto: "tcp", Local: "127.0.0.1:8080", Remote: gvproxy.GuestIP + ":8080"},
		{Proto: "tcp", Local: "127.0.0.1:8081", Remote: gvproxy.GuestIP + ":8081", Error: "address already in use"},
	}}
	if err := st.Save(forwarder.StatePath(m.Dir)); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "ports", "gv")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not running") || !strings.Contains(out, "LOCAL") ||
		!strings.Contains(out, "127.0.0.1:8080") || !strings.Contains(out, "error: address already in use") {
		t.Errorf("ports = %q", out)
	}
	out, err = run(t, root, "--json", "ports", "gv")
	if err != nil {
		t.Fatal(err)
	}
	var entries []forwarder.Entry
	if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) != 2 || entries[1].Error == "" {
		t.Errorf("json ports = %q, %v", out, err)
	}

	out, err = run(t, root, "inspect", "gv")
	if err != nil || !strings.Contains(out, "Forwarder:") || !strings.Contains(out, "Port:") || !strings.Contains(out, "127.0.0.1:8081 -> "+gvproxy.GuestIP+":8081 tcp (error: address already in use)") {
		t.Errorf("inspect = %q, %v", out, err)
	}
	out, err = run(t, root, "--json", "inspect", "gv")
	if err != nil {
		t.Fatal(err)
	}
	var i struct {
		Ports     []forwarder.Entry `json:"ports"`
		Forwarder string            `json:"forwarder_state"`
	}
	if err := json.Unmarshal([]byte(out), &i); err != nil || len(i.Ports) != 2 || i.Forwarder != "stopped" {
		t.Errorf("json inspect = %q, %v", out, err)
	}

	out, err = run(t, root, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasSuffix(strings.TrimSpace(lines[0]), "PORTS") || !strings.HasSuffix(strings.TrimSpace(lines[1]), "2") {
		t.Errorf("list = %q", out)
	}
}

// A machine with no forwards.json has an empty table, and the internal
// forwarder command is hidden from help.
func TestPortsEmptyAndHiddenForwarder(t *testing.T) {
	root := t.TempDir()
	seedGvproxyRecord(t, root, "gv")
	out, err := run(t, root, "--json", "ports", "gv")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json ports = %q, %v", out, err)
	}
	out, err = run(t, root, "--help")
	if err != nil || !strings.Contains(out, "\n  ports ") || strings.Contains(out, forwarder.Command) {
		t.Errorf("help = %q, %v", out, err)
	}
	if _, err := run(t, t.TempDir(), forwarder.Command, "nope"); err == nil {
		t.Error("_forwarder on a missing machine should fail")
	}
}

// Stopping a stopped machine whose forwarder left a stale pid file tidies
// it away and never touches the provider.
func TestStopTidiesStaleForwarderPID(t *testing.T) {
	root := t.TempDir()
	seedFakeRecord(t, root, "fk")
	fakeBE.state, fakeBE.stops = backend.Running, nil
	fakeNet.state, fakeNet.stops = backend.Running, 0
	pidFile := filepath.Join(root, "machines", "fk", forwarder.PIDFile)
	if err := os.WriteFile(pidFile, []byte("2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, root, "stop", "--force", "fk"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("stale forwarder pid file survived stop")
	}
}
