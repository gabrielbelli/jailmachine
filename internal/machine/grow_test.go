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
	for _, want := range []string{"diskinfo vtbd0", "-ge 85899345920", "gpart show -p vtbd0", "freebsd-zfs", "gpart recover vtbd0", `gpart resize -i "$idx" vtbd0`, `zpool online -e zroot vtbd0p"$idx"`} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in:\n%s", want, cmd)
		}
	}
}
