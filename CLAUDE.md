# jailmachine

`docker-machine` / `podman machine`-style CLI that provisions and manages a
FreeBSD VM for running **jails** (bastille) and **OCI containers** (podman —
native FreeBSD images, plus Linux images via the Linuxulator), and connects
the host's `podman` client to it.

## Decisions already made (2026-08-20)

- **Name**: `jailmachine` (one word), CLI alias `jm`. Subcommands mirror
  `podman machine`: `init`, `start`, `stop`, `ssh`, `inspect`, `rm`.
- **macOS backend: QEMU + HVF** (`qemu-system-aarch64 -M virt,accel=hvf -cpu host`,
  EDK2 pflash, virtio-blk/net/rng, slirp with `hostfwd` for SSH, serial to
  `console.log`, QMP socket). **Tried and rejected on 2026-08-20:**
  Apple Virtualization.framework / `vfkit` — FreeBSD/arm64 does not boot on it
  (kernel dies after `No valid device tree blob found!`; no fix exists, no UART
  either). Also not Lima (needs Linux guest agent), not `podman machine`
  (hard-wired to Fedora CoreOS + Ignition), not apple/container (Linux kernel only).
- **Future backends**: Linux (KVM/libvirt or plain QEMU), Windows (Hyper-V).
  Keep the backend behind an interface from day one.
- **Guest**: FreeBSD 15.x arm64 — a kernel runs jail/OCI userlands of its own
  version or older, never newer, so 15.x is required for 15.x images. Image:
  official `BASIC-CLOUDINIT-zfs.raw.xz` (has `nuageinit`; ZFS auto-grows to
  the 64 GB we truncate to). Provisioned on first boot by a `#!/bin/sh`
  `user-data` on a NoCloud ISO (`cidata` label, built with `hdiutil makehybrid`).
  Installs `bastille` + `podman-suite`, enables `pf`, `linux`, `zfs`,
  `zroot/containers` at `/var/db/containers`. Hostname `jailmachine`.
- **Gotchas learnt**: `nuageinit` runs before DHCP finishes → wait for DNS
  before `pkg`. The ports `podman` rc script only restarts containers; we ship
  our own `podman_service` rc script running `podman system service` on
  `/var/run/podman/podman.sock`. `pf.conf.sample` uses `v4egress_if`/`v6egress_if`.
  podman-remote trusts `~/.ssh/known_hosts` → `ssh-keygen -R '[127.0.0.1]:2222'`
  on `rm`/`start`. Linux images need `--os=linux` on the host podman.
- **Host ↔ guest**: SSH only, as root with a dedicated key, on
  `127.0.0.1:2222` (slirp `hostfwd`). Podman via
  `podman system connection add --default jailmachine ssh://root@127.0.0.1:2222/var/run/podman/podman.sock`.
- **Known FreeBSD limits** (document, don't fight): no virtiofs driver → no
  host bind-mounts (offer NFS/sshfs); no vsock → socket forwarded over SSH.
- Licence BSD-2-Clause. Git identity pinned to the personal profile
  (`.git/git-profile`).

## Conventions

- British English in prose.
- Runtime state lives in `~/.jailmachine/` (`disk.raw`, `seed.iso`, `efivars.fd`,
  `ssh/`, `machine.json`, `console.log`, `qemu.pid`, `qmp.sock`) — never in the repo. `.gitignore` already excludes images.
- Implementation language: not decided. Shell is fine for the PoC; Go is the
  natural fit for a cross-platform single binary later (vfkit itself is Go).

## First milestone — reached 2026-08-20

`bin/jm init && bin/jm start && podman run --rm --os=linux docker.io/alpine echo hi`
works on this Mac (Apple Silicon, macOS 26.5): first boot ≈2 min, warm boot
≈12 s. Native FreeBSD image verified too (`dougrabson/freebsd15-minimal`).

## Plan

MVP plan in `docs/MVP-PLAN.md`; architecture in `docs/adr/` (abstract: layers, interfaces, contracts); concrete tools in `docs/tech-choices.md` (read them before
changing architecture; add a new ADR rather than editing an accepted one).

## Next candidates

- `jm` subcommands: `list`, `set` (cpus/mem), `start --gui`/console attach.
- Port forwarding for `podman run -p` (SSH `-L` or slirp `hostfwd` on demand).
- vmnet/bridged networking so the VM has a routable IP.
- Host file access: NFS or sshfs helper; check `p9fs` (virtio-9p) on FreeBSD 15.
- Rewrite in Go behind a backend interface; add Linux/KVM backend.
