# Guest contract (ADR 0003)

What a guest must satisfy for `jm` to manage it, regardless of image source:

1. **Boots** from a raw disk with an EFI system partition; root filesystem grows to the disk on first boot (official `BASIC-CLOUDINIT-zfs` does this).
2. **Consumes a NoCloud seed** (ISO, label `cidata`) holding `meta-data` (`instance-id`, `local-hostname`) and a `#!/bin/sh` `user-data`, run once by `nuageinit` as root.
3. **After provisioning exposes**: `sshd` for the configured user (root, key-only), the podman API on `/var/run/podman/podman.sock` (our `podman_service` rc script), and the ready marker `/var/db/jm-provisioned`.
4. **Provisioning is idempotent** and logs to `/var/log/jm-provision.log`.

`provision.sh` is the single source of truth. The seed builder prepends
`JM_SSH_PUBKEY='...'` and `JM_HOSTNAME='...'` and ships the result as `user-data`;
prebaked release images are produced by running the very same script.

Known kernel limits: no virtiofs (no host bind-mounts), no vsock (API over SSH).
