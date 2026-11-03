#!/bin/sh
# Seal a fully provisioned guest so its disk can ship as a prebaked image.
# Run by "jm image build" over ssh as root, after provision.sh finished and
# the ready marker /var/db/jm-provisioned exists (it stays: it is what
# selects the fast path in provision.sh on the next machine).
# See docs/guest-contract.md.
set -x
# Host shares belong to the machine this image was built on, not to the
# image: unmount them, then remove the mountpoints jm_shares created and
# its block in /etc/fstab, so nothing of the builder's host is captured.
# The next machine's jm_shares writes its own at boot.
_jm_dirs=$(awk -F '	' '$1 != "" && $1 !~ /^#/ { print $2 }' /var/db/jm/conf/shares.tab 2>/dev/null)
service jm_shares stop || true
printf '%s\n' "$_jm_dirs" | tail -r | while IFS= read -r _dir; do
	[ -n "$_dir" ] && rmdir -p "$_dir" 2>/dev/null
done
rm -rf /var/db/jm
if [ -f /etc/fstab ]; then
	awk '
		$0 == "# BEGIN jm shares (generated, do not edit)" { skip = 1 }
		!skip { print }
		$0 == "# END jm shares" { skip = 0 }
	' /etc/fstab > /etc/fstab.seal && chmod 644 /etc/fstab.seal &&
	    mv -f /etc/fstab.seal /etc/fstab
fi
# Name resolution: the dns stage of the build machine's "jm start" pointed
# the guest at that host's resolver port and search domains (ADR 0008).
# Undo it, so the next machine boots on DHCP's resolv.conf until its own
# dns stage takes over, and a first start whose dns stage fails is a
# warning rather than a failure.
rm -f /etc/resolv.conf /var/unbound/unbound.conf
rm -f /usr/local/etc/containers/containers.conf.d/50-jailmachine-dns.conf
[ -f /etc/resolvconf.conf ] && sed -i '' -e '/# jailmachine owns /d' /etc/resolvconf.conf || true
sysrc -x local_unbound_enable || true
# Per-machine identity: the next machine's seed brings its own key and
# hostname; sshd regenerates host keys on start (sshd_keygen precmd).
rm -f /root/.ssh/authorized_keys
rm -f /etc/ssh/ssh_host_*
sysrc -x hostname || true
# The ZFS ARC cap is a machine setting jm writes at every start, not part of
# the image (sysrc(8) cannot edit it: the name has dots in it).
[ -f /boot/loader.conf ] && sed -i '' -e '/^[[:space:]]*vfs\.zfs\.arc\.max[[:space:]]*=/d' /boot/loader.conf || true
# Logs and state of this build's first boot.
rm -f /var/log/jm-provision.log /var/db/jm-provision-failed /var/log/nuageinit.log
find /var/log -type f -exec truncate -s 0 {} +
rm -rf /var/cache/nuageinit
rm -rf /root/.history /root/.ssh/known_hosts /tmp/* /var/tmp/*
# Host identity and entropy must be per machine: hostid is regenerated on
# boot when missing, and the seeds are re-created by the entropy rc script.
rm -f /etc/hostid /entropy
rm -rf /var/db/entropy/*
pkg clean -ay || true
# nuageinit only runs on a boot where the /firstboot sentinel exists (rc(8)
# skips every "KEYWORD: firstboot" script otherwise and removes the
# sentinel at the end of the boot); the instance-id is never consulted.
# Put it back so the next machine's seed is applied. The pkgbase upgrade
# that also hangs off firstboot was disabled by provision.sh (rc.conf).
touch /firstboot
sysrc firstboot_pkg_upgrade_enable=NO
# Hand freed blocks back to the host as holes (QEMU discard=unmap). No
# zero-fill: the stock image has no ZFS compression, so writing ~60 GiB of
# real zeros would land in the host's sparse disk.raw (ENOSPC on a small
# runner) before trim could punch them out again; trim alone does the job.
zpool trim -w zroot || zpool trim zroot || true
sync
echo "jm-seal: done"
