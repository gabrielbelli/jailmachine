package qemu

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func TestMachineTypeOf(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{Binary, "-M", "virt,accel=hvf", "-m", "2048"}, "virt"},
		{[]string{Binary, "-M", "virt-11.1,accel=hvf"}, "virt-11.1"},
		{[]string{Binary, "-machine", "type=virt-10.0,accel=kvm"}, "virt-10.0"},
		{[]string{Binary, "-M", "virt"}, "virt"},
	} {
		if got, err := MachineTypeOf(tc.argv); err != nil || got != tc.want {
			t.Errorf("MachineTypeOf(%q) = %q, %v; want %q", tc.argv, got, err, tc.want)
		}
	}
	for _, bad := range [][]string{{Binary, "-m", "2048"}, {Binary, "-M"}, {Binary, "-M", ",accel=hvf"}} {
		if got, err := MachineTypeOf(bad); err == nil {
			t.Errorf("MachineTypeOf(%q) = %q, want an error", bad, got)
		}
	}
}

func TestPinMachineTypeBareAliasResolvesViaQMP(t *testing.T) {
	sock, seen := scriptedQMP(t, func(c qmpCommand) []any {
		if c.Execute == "query-machines" {
			return []any{ret([]map[string]any{{"name": "virt-11.0"}, {"name": "virt-11.1", "alias": "virt"}, {"name": "sbsa-ref"}})}
		}
		return []any{ret(map[string]any{})}
	})
	ctx := context.Background()
	q, err := DialMonitor(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := PinMachineType(ctx, []string{Binary, "-M", "virt,accel=hvf"}, q); err != nil || got != "virt-11.1" {
		t.Fatalf("PinMachineType = %q, %v; want virt-11.1", got, err)
	}
	// An alias this QEMU does not know leaves nothing to pin: unavailable.
	if got, err := PinMachineType(ctx, []string{Binary, "-M", "sbsa-ref"}, q); err == nil {
		t.Fatalf("PinMachineType of an unversioned, unaliased type = %q, want an error", got)
	}
	q.Close()
	if cmds := <-seen; len(cmds) != 3 || cmds[1].Execute != "query-machines" {
		t.Fatalf("server saw %v", cmds)
	}
}

// A machine resumed once carries virt-11.1 in its argv; after a QEMU upgrade
// the alias points at virt-11.2, but the next suspend keeps virt-11.1.
func TestPinMachineTypeKeepsVersionedAfterUpgrade(t *testing.T) {
	sock, seen := scriptedQMP(t, func(c qmpCommand) []any {
		if c.Execute == "query-machines" {
			return []any{ret([]map[string]any{{"name": "virt-11.2", "alias": "virt"}, {"name": "virt-11.1"}})}
		}
		return []any{ret(map[string]any{})}
	})
	ctx := context.Background()
	q, err := DialMonitor(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{Binary, "-M", "virt-11.1,accel=hvf", "-incoming", "defer"}
	if got, err := PinMachineType(ctx, argv, q); err != nil || got != "virt-11.1" {
		t.Fatalf("PinMachineType = %q, %v; want virt-11.1", got, err)
	}
	q.Close()
	for _, c := range <-seen {
		if c.Execute == "query-machines" {
			t.Fatal("a versioned machine type must not be resolved again")
		}
	}
}

func TestIncomingArgv(t *testing.T) {
	saved := []string{"/opt/homebrew/bin/" + Binary, "-M", "virt,accel=hvf", "-m", "2048",
		"-fsdev", "local,id=jmfs0,path=/Users/me/src,security_model=mapped-xattr,multidevs=remap",
		"-device", "virtio-9p-pci,fsdev=jmfs0,mount_tag=jm0,addr=0x9",
		"-pidfile", "/state/qemu.pid"}
	got, err := IncomingArgv(saved, "virt-11.1")
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(saved)
	want[2] = "virt-11.1,accel=hvf"
	want = append(want, "-incoming", "defer")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IncomingArgv\n got: %q\nwant: %q", got, want)
	}
	if saved[2] != "virt,accel=hvf" {
		t.Fatal("IncomingArgv modified its input")
	}
	// A previously resumed argv: the old -incoming defer is stripped, not
	// doubled, and an already pinned type is re-pinned in place.
	again, err := IncomingArgv(got, "virt-11.1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("IncomingArgv of a resumed argv\n got: %q\nwant: %q", again, want)
	}
	if n := strings.Count(strings.Join(again, " "), "-incoming"); n != 1 {
		t.Fatalf("-incoming appears %d times: %q", n, again)
	}
	if !slices.Contains(again, "virtio-9p-pci,fsdev=jmfs0,mount_tag=jm0,addr=0x9") {
		t.Fatalf("share device changed: %q", again)
	}
	if _, err := IncomingArgv(append(slices.Clone(saved), "-daemonize"), "virt-11.1"); err == nil {
		t.Fatal("-daemonize must be refused")
	}
	if _, err := IncomingArgv(append(slices.Clone(saved), "-incoming", "tcp:0:4444"), "virt-11.1"); err == nil {
		t.Fatal("an -incoming other than defer must be refused")
	}
	if _, err := IncomingArgv([]string{Binary, "-m", "1"}, "virt-11.1"); err == nil {
		t.Fatal("an argv without -M must be refused")
	}
}

func TestResolveSavedArgvPlaceholderAndFirmware(t *testing.T) {
	dir := t.TempDir()
	kept := t.TempDir()
	gone := filepath.Join(t.TempDir(), "unplugged")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	m := shareMachine(t, kept, gone+":ro")
	var goneShare machine.Share
	for _, s := range m.Shares {
		if s.HostPath != kept {
			goneShare = s
		}
	}
	p := samplePaths(dir)
	p.GuestConf = filepath.Join(dir, machine.GuestConfDir)
	if err := os.MkdirAll(p.GuestConf, 0o755); err != nil {
		t.Fatal(err)
	}
	p.Code = filepath.Join(t.TempDir(), "old-cellar", FirmwareCode) // moved by an upgrade
	fwDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fwDir, FirmwareCode), []byte("fw"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := append([]string{Binary}, Args(m, backend.NetAttachment{Kind: backend.KindUser, HostFwdSSH: 2222}, p)...)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	got, absent, err := resolveSavedArgv(saved, dir, fwDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(absent, []string{goneShare.Tag}) {
		t.Fatalf("absent = %q, want [%s]", absent, goneShare.Tag)
	}
	placeholder := filepath.Join(dir, machine.GuestConfDir, AbsentShareDir, goneShare.Tag)
	if st, err := os.Stat(placeholder); err != nil || !st.IsDir() {
		t.Fatalf("placeholder not created: %v", err)
	}
	if len(got) != len(saved) {
		t.Fatalf("argv length changed: %d -> %d", len(saved), len(got))
	}
	for i := range saved {
		switch {
		case strings.Contains(saved[i], "path="+gone):
			if !strings.Contains(got[i], "path="+placeholder) || !strings.Contains(got[i], "readonly=on") ||
				!strings.HasPrefix(got[i], "local,id=jmfs") {
				t.Errorf("vanished share not replaced: %q", got[i])
			}
		case strings.Contains(saved[i], "file="+p.Code):
			if got[i] != strings.Replace(saved[i], p.Code, filepath.Join(fwDir, FirmwareCode), 1) {
				t.Errorf("moved firmware not resolved: %q", got[i])
			}
		default:
			if got[i] != saved[i] {
				t.Errorf("argv[%d] changed: %q -> %q", i, saved[i], got[i])
			}
		}
	}
	// Nothing to resolve: the argv comes back unchanged.
	again, absent, err := resolveSavedArgv(got, dir, fwDir)
	if err != nil || len(absent) != 0 || !reflect.DeepEqual(again, got) {
		t.Fatalf("second pass changed the argv: %q, %q, %v", again, absent, err)
	}
}

func TestSplitOptsKeepsEscapedCommas(t *testing.T) {
	opts := splitOpts("local,id=jmfs0,path=/a,,b,readonly=on")
	if !reflect.DeepEqual(opts, []string{"local", "id=jmfs0", "path=/a,,b", "readonly=on"}) {
		t.Fatalf("splitOpts = %q", opts)
	}
	if got := optGet(opts, "path"); got != "/a,b" {
		t.Fatalf("optGet = %q", got)
	}
	if got := strings.Join(optSet(opts, "path", "/c,d"), ","); got != "local,id=jmfs0,path=/c,,d,readonly=on" {
		t.Fatalf("optSet = %q", got)
	}
}
