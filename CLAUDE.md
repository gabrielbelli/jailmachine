# jailmachine

`docker-machine` / `podman machine`-style CLI that provisions and manages a
FreeBSD VM for running **jails** (bastille) and **OCI containers** (podman —
native FreeBSD images, plus Linux images via the Linuxulator), and connects
the host's `podman` client to it.

## Decisions already made (2026-08-20)

- **Name**: `jailmachine` (one word), CLI alias `jm`. Subcommands mirror
  `podman machine`: `init`, `start`, `stop`, `ssh`, `inspect`, `rm`.
- **macOS backend first**: Apple Virtualization.framework via
  [`vfkit`](https://github.com/crc-org/vfkit) (`brew install vfkit`). **Not**
  Parallels, not QEMU, not Lima (Lima needs a Linux guest agent; `podman
  machine` is hard-wired to Fedora CoreOS + Ignition).
- **Future backends**: Linux (KVM/libvirt or plain QEMU), Windows (Hyper-V).
  Keep the backend behind an interface from day one.
- **Guest**: FreeBSD 14.x arm64 official `.raw` VM image, grown to 64 GB,
  ZFS root preferred. Install `bastille` + `podman-suite`, enable `pf`,
  `linux` compat, `zfs`. Hostname `jailmachine`.
- **Host ↔ guest**: SSH only. Podman via
  `podman system connection add jailmachine ssh://<user>@<ip>/var/run/podman/podman.sock`.
- **Known FreeBSD limits** (document, don't fight): no virtiofs driver → no
  host bind-mounts (offer NFS/sshfs); no vsock → socket forwarded over SSH.
- Licence BSD-2-Clause. Git identity pinned to the personal profile
  (`.git/git-profile`).

## Conventions

- British English in prose.
- Runtime state lives in `~/.jailmachine/` (`disk.raw`, `efistore`, `ssh/`,
  `machine.json`) — never in the repo. `.gitignore` already excludes images.
- Implementation language: not decided. Shell is fine for the PoC; Go is the
  natural fit for a cross-platform single binary later (vfkit itself is Go).

## First milestone

`jm init && jm start && podman --connection jailmachine run --rm docker.io/alpine echo hi`
working on this Mac (Apple Silicon, macOS 26.5).
