# Guest contract (ADR 0003)

What a guest must satisfy for `jm` to manage it, regardless of image source:

1. **Boots** from a raw disk with an EFI system partition; root filesystem grows to the disk on first boot (official `BASIC-CLOUDINIT-zfs` does this).
2. **Consumes a NoCloud seed** (ISO, label `cidata`) holding `meta-data` (`instance-id`, `local-hostname`) and a `#!/bin/sh` `user-data`, run once by `nuageinit` as root.
3. **After provisioning exposes**: `sshd` for the configured user (root, key-only), the podman API on `/var/run/podman/podman.sock` (our `podman_service` rc script), and the ready marker `/var/db/jm-provisioned`. The script runs under `set -e`: on failure it writes `/var/db/jm-provision-failed` instead, which `jm start` reports straight away. The official image's `firstboot_pkg_upgrade` is disabled and its reboot request cancelled (it must not fire mid-script); when it installed a new kernel (`freebsd-version -k` differs from `-r`) `jm start` reboots the guest once after the ready marker appears, so the Linuxulator modules (`linux64`, `linprocfs`) load before podman is connected. The script asserts those modules are loaded whenever the running kernel is current.
4. **Provisioning is idempotent** and logs to `/var/log/jm-provision.log`.

`provision.sh` is the single source of truth. The seed builder prepends
`JM_SSH_PUBKEY='...'` and `JM_HOSTNAME='...'` and ships the result as `user-data`;
prebaked release images are produced by running the very same script.

Known kernel limits: no virtiofs and no vsock. Host directories are shared
over `virtio-9p` instead (ADR 0007) and the podman API is forwarded over SSH;
the paths the guest must provide for sharing, and for the host-resolver DNS
of ADR 0008, are in `docs/guest-contract.md`.

Known Linuxulator limits (the verified Docker Hub matrix is in
`docs/USAGE.md`): `linux_epoll` does not implement `EPOLLEXCLUSIVE` and
returns `EINVAL`, which is what kills stock `docker.io/nginx` — with
`worker_processes > 1` nginx registers its listening socket with that flag
and every worker dies with `epoll_ctl(1, 6) failed (22: Invalid argument)`.
`accept_mutex on;` in the `events` block (or `worker_processes 1;`, or
`reuseport`) makes it work; a ready-made image is `demo/nginx-linuxulator`.
Linux AIO is absent too (`io_setup` returns `ENOSYS`), but that one is
benign: nginx logs `io_setup() failed (38: Function not implemented)` once,
disables file AIO and serves normally. `redis-server` needs
`--ignore-warnings ARM64-COW-BUG`, and `docker.io/node` runs only far enough
to print `node --version`: every other invocation hangs, because
`linux_mremap` cannot grow a mapping and node's allocator retries forever
(`node:22-bookworm-slim`, on glibc, works). UDP is fine — Linux containers bind, send, receive and publish
`/udp` ports normally — except for busybox's `nc -u -l`, which peeks its
peer with a zero-length `recvmsg()`; that returns at once on FreeBSD where
Linux blocks, so it dies with `EAFNOSUPPORT`. Any other UDP server
(`netcat-openbsd`, `socat`) works. A port published with a host address
(`-p 127.0.0.1:8080:80`) binds that address on the Mac: the forwarder loads
an `rdr` rule into the guest's own `rdr/jm` pf anchor to make it so, and
`jm ports` shows the resulting mapping.
