package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov/gvproxy"
)

func TestParseMemoryMiB(t *testing.T) {
	good := map[string]int{
		"4096": 4096, "4096MiB": 4096, "4096 MiB": 4096, "4096m": 4096, "4096MB": 4096,
		"4GiB": 4096, "4g": 4096, "4G": 4096, "4GB": 4096, "  2 gib ": 2048, "256": 256,
	}
	for in, want := range good {
		got, err := ParseMemoryMiB(in)
		if err != nil || got != want {
			t.Errorf("ParseMemoryMiB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "4k", "4KiB", "4TiB", "g", "-4g", "4.5g", "99999999999999999999"} {
		if got, err := ParseMemoryMiB(in); err == nil {
			t.Errorf("ParseMemoryMiB(%q) = %d, want error", in, got)
		}
	}
}

func TestSetValidate(t *testing.T) {
	m := machine.Defaults()
	cases := []struct {
		name string
		o    setOpts
		want string // substring of the error; "" means valid
	}{
		{"nothing", setOpts{}, "nothing to set"},
		{"cpus ok", setOpts{cpus: 2, cpusSet: true}, ""},
		{"cpus zero", setOpts{cpus: 0, cpusSet: true}, "--cpus"},
		{"cpus huge", setOpts{cpus: 1000, cpusSet: true}, "--cpus"},
		{"memory ok", setOpts{memory: "8GiB", memorySet: true}, ""},
		{"memory small", setOpts{memory: "128", memorySet: true}, "--memory must"},
		{"memory junk", setOpts{memory: "lots", memorySet: true}, "--memory:"},
		{"disk grow", setOpts{disk: 128, diskSet: true}, ""},
		{"disk same", setOpts{disk: 64, diskSet: true}, ""},
		{"disk shrink", setOpts{disk: 32, diskSet: true}, "only grow"},
		{"disk zero", setOpts{disk: 0, diskSet: true}, "--disk must"},
		{"port ok", setOpts{sshPort: 2223, sshPortSet: true}, ""},
		{"port bad", setOpts{sshPort: 70000, sshPortSet: true}, "--ssh-port"},
		// The publish address is validated where it is typed, so a typo
		// is a usage error rather than a per-mapping expose failure once
		// a container is running (ADR 0004).
		{"publish ok", setOpts{publishAddr: "127.0.0.1", publishAddrSet: true}, ""},
		{"publish wildcard", setOpts{publishAddr: "0.0.0.0", publishAddrSet: true}, ""},
		{"publish default", setOpts{publishAddr: "", publishAddrSet: true}, ""},
		{"publish junk", setOpts{publishAddr: "loclahost", publishAddrSet: true}, "--publish-addr"},
		{"publish with port", setOpts{publishAddr: "127.0.0.1:8080", publishAddrSet: true}, "--publish-addr"},
	}
	for _, tc := range cases {
		c, err := tc.o.validate(&m)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
		if tc.name == "memory ok" && c.memoryMiB != 8192 {
			t.Errorf("memory ok: parsed %d", c.memoryMiB)
		}
	}
	c, _ := setOpts{disk: 128, diskSet: true}.validate(&m)
	if c.needsStopped() {
		t.Error("--disk alone should not need a stopped machine")
	}
	c, _ = setOpts{cpus: 2, cpusSet: true}.validate(&m)
	if !c.needsStopped() {
		t.Error("--cpus should need a stopped machine")
	}
}

func TestSetStoppedMachine(t *testing.T) {
	root := t.TempDir()
	seedRecord(t, root, "alpha")
	s := machine.NewStore(root)
	disk := s.Path("alpha", machine.DiskFile)
	if err := os.WriteFile(disk, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, root, "set", "alpha", "--cpus", "2", "--memory", "2g", "--ssh-port", "2300", "--disk", "65")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	m, err := s.Load("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if m.CPUs != 2 || m.MemoryMiB != 2048 || m.SSHPort != 2300 || m.DiskGiB != 65 {
		t.Errorf("record not updated: %+v", m)
	}
	if !m.PendingGrow() {
		t.Error("pending grow not recorded on a stopped machine")
	}
	st, err := os.Stat(disk)
	if err != nil || st.Size() != 65<<30 {
		t.Errorf("disk.raw size = %d, %v; want %d", st.Size(), err, 65<<30)
	}
	if !strings.Contains(out, "next start") {
		t.Errorf("output should mention the deferred grow:\n%s", out)
	}

	if _, err := run(t, root, "set", "alpha", "--disk", "32"); err == nil || !strings.Contains(err.Error(), "only grow") {
		t.Errorf("shrink accepted: %v", err)
	}
	if _, err := run(t, root, "set", "alpha"); err == nil {
		t.Error("no flags accepted")
	}
	if _, err := run(t, root, "set", "nope", "--cpus", "1"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing machine: %v", err)
	}
}

func TestFinishPendingGrowNoop(t *testing.T) {
	m := machine.Defaults()
	// No flag: must not touch the (nil) client.
	finishPendingGrow(context.Background(), &m, nil)
}

// $JM_PUBLISH_ADDR is an override read once, at start, and written onto the
// record: the forwarder runs detached, so an address left in the
// environment of whichever shell booted the machine would be invisible to
// "jm inspect" and would be lost the next time anyone ran a plain
// "jm start" (ADR 0004).
func TestApplyPublishAddrEnv(t *testing.T) {
	old := stateRoot
	defer func() { stateRoot = old }()
	stateRoot = t.TempDir()

	m := machine.Defaults()
	m.Name = "pub"
	if err := store().Save(&m); err != nil {
		t.Fatal(err)
	}

	// Unset: the record stands.
	t.Setenv(forwarder.PublishAddrEnv, "")
	os.Unsetenv(forwarder.PublishAddrEnv)
	if err := applyPublishAddrEnv(&m); err != nil || m.PublishAddr != "" {
		t.Fatalf("no variable: %q, %v", m.PublishAddr, err)
	}

	t.Setenv(forwarder.PublishAddrEnv, "127.0.0.1")
	if err := applyPublishAddrEnv(&m); err != nil {
		t.Fatal(err)
	}
	if m.PublishAddr != "127.0.0.1" {
		t.Errorf("PublishAddr = %q", m.PublishAddr)
	}
	// It is persisted, not just held in memory.
	reloaded, err := store().Load("pub")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PublishAddr != "127.0.0.1" {
		t.Errorf("saved PublishAddr = %q", reloaded.PublishAddr)
	}

	t.Setenv(forwarder.PublishAddrEnv, "not-an-ip")
	if err := applyPublishAddrEnv(reloaded); err == nil {
		t.Error("a bad address was accepted")
	}
}

// The publish address a running forwarder binds is the one it started with,
// not whatever the record says now: "jm set --publish-addr" takes effect on
// the next start, so "jm ports" and "jm inspect" must show the live value
// and mark the record's as pending rather than quietly claim it.
func TestPublishAddrsMarksAPendingChange(t *testing.T) {
	cases := []struct {
		name           string
		record         string
		running        bool
		state          *forwarder.State
		force, pending string
	}{
		{"stopped forwarder shows the record", "127.0.0.1", false,
			&forwarder.State{PublishAddr: "0.0.0.0"}, "127.0.0.1", ""},
		{"running forwarder wins over a changed record", "127.0.0.1", true,
			&forwarder.State{PublishAddr: "0.0.0.0"}, "0.0.0.0", "127.0.0.1"},
		{"agreement is not a pending change", "127.0.0.1", true,
			&forwarder.State{PublishAddr: "127.0.0.1"}, "127.0.0.1", ""},
		{"a forwards.json without the address claims nothing", "127.0.0.1", true,
			&forwarder.State{}, "127.0.0.1", ""},
		{"the default is compared, not the empty record", "", true,
			&forwarder.State{PublishAddr: "127.0.0.1"}, "127.0.0.1", forwarder.DefaultHostIP},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &machine.Machine{Name: "t", PublishAddr: c.record}
			force, pending := publishAddrs(m, c.running, c.state)
			if force != c.force || pending != c.pending {
				t.Errorf("publishAddrs = (%q, %q), want (%q, %q)", force, pending, c.force, c.pending)
			}
		})
	}
}

// "jm ports" prints the default publish address as a default, because the
// rows no longer all share it: a publish that names a host address binds
// that address and shows it in LOCAL.
func TestPortsHeaderIsTheDefaultNotTheWholeTruth(t *testing.T) {
	root := t.TempDir()
	m := seedGvproxyRecord(t, root, "gv")
	m.PublishAddr = "0.0.0.0"
	if err := machine.NewStore(root).Save(m); err != nil {
		t.Fatal(err)
	}
	st := &forwarder.State{PublishAddr: "0.0.0.0", Owned: []forwarder.Entry{
		{Proto: "tcp", Local: "0.0.0.0:8080", Remote: gvproxy.GuestIP + ":8080"},
		{Proto: "tcp", Local: "127.0.0.1:8082", Remote: gvproxy.GuestIP + ":8082"},
		{Proto: "tcp", Local: "[::1]:8087", Remote: gvproxy.GuestIP + ":8087"},
		{Proto: "udp", Local: "127.0.0.1:8095", Remote: gvproxy.GuestIP + ":8095",
			Error: "the container has no address on the guest's container network yet"},
	}}
	if err := st.Save(forwarder.StatePath(m.Dir)); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, root, "ports", "gv")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# publishing on 0.0.0.0 unless -p names a host address") {
		t.Errorf("header does not read as a default:\n%s", out)
	}
	for _, want := range []string{"0.0.0.0:8080", "127.0.0.1:8082", "[::1]:8087", "udp", "no address"} {
		if !strings.Contains(out, want) {
			t.Errorf("ports output lacks %q:\n%s", want, out)
		}
	}
	// Every row is forwarded now; the "-" column is for nothing podman
	// can produce.
	if strings.Contains(out, "\t-\t") || strings.Contains(out, " -  ") {
		t.Errorf("a mapping was reported as unforwardable:\n%s", out)
	}
}
