package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/machine"
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
