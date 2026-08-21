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
// re-reads the GPT backup header (gpart recover), extends the partition
// zroot lives on (the vdev is read from "zpool list -vHP", not assumed to
// be a fixed vtbd0pN) to the end of the disk, and lets zroot claim the new
// space ("zpool online -e"; FreeBSD does not autoexpand a running pool).
// It first checks that the hypervisor really presents at least size bytes
// (diskinfo), so a grow the VM has not picked up fails loudly instead of
// no-op'ing, and it fails unless the pool really grew; only a resize with
// nothing to do (gpart "Nothing to do", or a pool with no expandable
// space, i.e. an earlier grow that was already applied) is tolerated.
func GuestGrowCmd(size int64) string {
	d := GuestGrowDisk
	return fmt.Sprintf(`set -e
have=$(diskinfo %[1]s | awk '{print $3}')
[ "${have:-0}" -ge %[2]d ] || { echo "%[1]s is ${have:-?} bytes, expected at least %[2]d: the hypervisor still presents the old size" >&2; exit 1; }
vdev=$(zpool list -vHP zroot | awk 'NR > 1 && $1 ~ "^/dev/%[1]sp[0-9]+$" { print $1; exit }')
[ -n "$vdev" ] || { echo "zroot has no vdev on %[1]s" >&2; exit 1; }
part=${vdev#/dev/}
idx=${part#%[1]sp}
gpart show -p %[1]s | awk -v p="$part" '$3 == p && $4 == "freebsd-zfs" { found = 1 } END { exit !found }' || { echo "$part is not a freebsd-zfs partition on %[1]s" >&2; exit 1; }
gpart recover %[1]s >/dev/null 2>&1 || true
if ! out=$(gpart resize -i "$idx" %[1]s 2>&1); then
	case "$out" in
	*[Nn]othing\ to\ do*) ;;
	*) echo "gpart resize: $out" >&2; exit 1 ;;
	esac
fi
before=$(zpool list -Hp -o size zroot)
zpool online -e zroot "$vdev"
after=$(zpool list -Hp -o size zroot)
expand=$(zpool get -Hp -o value expandsize zroot)
[ "$after" -gt "$before" ] || [ "$expand" = "-" ] || [ "$expand" = "0" ] || { echo "zroot is still $after bytes after zpool online -e ($expand bytes expandable)" >&2; exit 1; }`, d, size)
}
