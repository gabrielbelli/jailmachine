#!/bin/sh
# Seal a fully provisioned guest so its disk can ship as a prebaked image.
# Run by "jm image build" over ssh as root, after provision.sh finished and
# the ready marker /var/db/jm-provisioned exists (it stays: it is what
# selects the fast path in provision.sh on the next machine).
# See docs/guest-contract.md.
set -x
# Host shares belong to the machine this image was built on, not to the
# image: unmount them so nothing of the builder's host is captured.
service jm_shares stop || true
rm -rf /var/db/jm
# Per-machine identity: the next machine's seed brings its own key and
# hostname; sshd regenerates host keys on start (sshd_keygen precmd).
rm -f /root/.ssh/authorized_keys
rm -f /etc/ssh/ssh_host_*
sysrc -x hostname || true
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
