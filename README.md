# jailmachine

A `docker-machine`-style experience for FreeBSD: one command brings up a
lightweight FreeBSD VM that runs **jails** (via `bastille`) and **OCI
containers** (via `podman`, native FreeBSD images and Linux images through
the Linuxulator), then wires your host's `podman` client to it.

```bash
brew install qemu podman xz
git clone https://github.com/gabrielbelli/jailmachine && cd jailmachine

bin/jm init      # download FreeBSD 15.1 image (~600 MB), build the seed, keys
bin/jm start     # first boot ≈2 min (installs podman + bastille), later ≈12 s
podman run --rm --os=linux docker.io/alpine echo hi          # Linux image
podman run --rm docker.io/dougrabson/freebsd15-minimal uname -srm  # native FreeBSD image
bin/jm ssh       # root shell in the VM; `bastille` is there
bin/jm stop
bin/jm rm
```

`jm start` registers (and makes default) a `podman system connection` named
`jailmachine`, so the host `podman` talks to the VM. Use `--os=linux` for Linux
images: the guest is FreeBSD, so podman pulls FreeBSD variants by default.

## Status

Proof of concept, shell only. macOS on Apple Silicon, **QEMU + HVF**
(`qemu-system-aarch64 -M virt -accel hvf`). Verified 2026-08-20: clean
`init → start → podman run` for both a Linux image and a native FreeBSD 15
image. Linux (KVM) and Windows (Hyper-V / WSL2) backends are planned; the
QEMU command line ports to KVM almost unchanged.

> **Why not Apple's Virtualization.framework (vfkit)?** FreeBSD/arm64 does not
> boot on it: the kernel dies right after the loader
> (`No valid device tree blob found!`, then silence). Tested with 15.1-RELEASE
> and confirmed by several independent reports. It also has no UART, so there
> would be no serial console. QEMU gives us a real console in
> `~/.jailmachine/console.log`.

State lives in `~/.jailmachine/` (`disk.raw`, `seed.iso`, `efivars.fd`,
`ssh/`, `machine.json`, `console.log`). Tunables: `JM_CPUS`, `JM_MEM` (MiB),
`JM_DISK`, `JM_SSH_PORT` (default 2222), `JM_RELEASE`.

## Why

Think **Docker Desktop / `docker-machine`, but for FreeBSD**: the same
"install, run one command, `podman run` just works" experience on a Mac or
Windows box, except the engine is a FreeBSD VM. That gives you the new native
FreeBSD OCI images *and* Linux images (via the Linuxulator) side by side,
plus classic jails, from your normal host `podman` client.

`podman machine` is hard-wired to Fedora CoreOS. If you want a FreeBSD
userland — jails, pf, ZFS, native FreeBSD OCI images — there is no
one-command equivalent. This is that.

## Known limits (FreeBSD kernel, not ours)

- No virtiofs driver → no host bind-mounts; volumes live in the VM (NFS/sshfs
  for host files).
- No vsock driver → the podman socket is forwarded over SSH.
- Linuxulator gaps hit some Linux images: nginx workers die with
  `epoll_ctl … Invalid argument`; busybox httpd, alpine, most CLI tools are
  fine. Native FreeBSD images have no such limits.
- Networking is QEMU user-mode (slirp): fine for pulls and `podman run -p`
  via SSH forwarding later; not a routable VM IP. vmnet is a later step.

## Licence

BSD-2-Clause.
