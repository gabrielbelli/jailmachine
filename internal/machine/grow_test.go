package machine

import (
	"strings"
	"testing"
)

func TestPendingGrow(t *testing.T) {
	var m Machine // nil BackendOpts
	if m.PendingGrow() {
		t.Fatal("pending on a fresh record")
	}
	m.SetPendingGrow(true)
	if !m.PendingGrow() || m.BackendOpts[PendingGrowKey] != "1" {
		t.Fatalf("not recorded: %v", m.BackendOpts)
	}
	m.SetPendingGrow(false)
	if _, ok := m.BackendOpts[PendingGrowKey]; ok || m.PendingGrow() {
		t.Fatalf("not cleared: %v", m.BackendOpts)
	}
}

func TestGuestGrowCmd(t *testing.T) {
	cmd := GuestGrowCmd(80 << 30)
	for _, want := range []string{"diskinfo vtbd0", "-ge 85899345920", "zpool list -vHP zroot", "^/dev/vtbd0p[0-9]+$", "gpart show -p vtbd0", "freebsd-zfs", "gpart recover vtbd0", `gpart resize -i "$idx" vtbd0`, `othing\ to\ do`, "zpool list -Hp -o size zroot", `zpool online -e zroot "$vdev"`, "expandsize", `[ "$after" -gt "$before" ]`} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "|| true\nzpool") || strings.Contains(cmd, `vtbd0p"$idx"`) {
		t.Errorf("resize failures must not be ignored and the vdev must not be hard-coded:\n%s", cmd)
	}
}
