package machine

import "fmt"

// PendingGrowKey is the BackendOpts flag recording that disk.raw was grown
// while the machine was stopped and the guest's partition table and ZFS
// pool still have to be told (consumed by "jm start" once sshd answers).
const PendingGrowKey = "pending_grow"

// PendingGrow reports whether a guest-side grow is outstanding.
func (m *Machine) PendingGrow() bool {
	return m.BackendOpts[PendingGrowKey] == "1"
}

// SetPendingGrow records or clears the outstanding grow.
func (m *Machine) SetPendingGrow(pending bool) {
	if m.BackendOpts == nil {
		m.BackendOpts = map[string]string{}
	}
	if pending {
		m.BackendOpts[PendingGrowKey] = "1"
		return
	}
	delete(m.BackendOpts, PendingGrowKey)
}

// GuestGrowDisk is the virtio disk device the root pool lives on (ADR 0003:
// the official image has a single virtio-blk disk).
const GuestGrowDisk = "vtbd0"

// GuestGrowCmd is the shell run over SSH after disk.raw has been grown: it
// re-reads the GPT backup header (gpart recover), extends the freebsd-zfs
// partition, found by type rather than by a fixed index, to the end of the
// disk, and lets zroot claim the new space ("zpool online -e"; FreeBSD does
// not autoexpand a running pool). It first checks that the hypervisor
// really presents at least size bytes (diskinfo), so a grow the VM has not
// picked up fails loudly instead of no-op'ing. A resize that has nothing to
// do is not a failure.
func GuestGrowCmd(size int64) string {
	d := GuestGrowDisk
	return fmt.Sprintf(`set -e
have=$(diskinfo %[1]s | awk '{print $3}')
[ "${have:-0}" -ge %[2]d ] || { echo "%[1]s is ${have:-?} bytes, expected at least %[2]d: the hypervisor still presents the old size" >&2; exit 1; }
idx=$(gpart show -p %[1]s | awk '$4 == "freebsd-zfs" { sub("^%[1]sp", "", $3); print $3; exit }')
[ -n "$idx" ] || { echo "no freebsd-zfs partition on %[1]s" >&2; exit 1; }
gpart recover %[1]s >/dev/null 2>&1 || true
gpart resize -i "$idx" %[1]s || true
zpool online -e zroot %[1]sp"$idx"`, d, size)
}
