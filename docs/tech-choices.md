# Current technology choices (not architecture)

These are implementations of the ADR interfaces as of 2026-08-20. Swap them
without touching the ADRs.

| Interface (ADR) | Current choice | Why / notes |
|---|---|---|
| Host client language | Go (cobra) | Single binary; same ecosystem as podman/gvproxy |
| Backend, macOS (0002) | QEMU + HVF, `-M virt`, EDK2 pflash, QMP | Apple Virtualization.framework/vfkit **cannot boot FreeBSD/arm64** (kernel dies after loader; no UART either) — verified 2026-08-20 and by independent reports |
| Backend, Linux (0002) | QEMU + KVM (planned) | Same argv, different accel |
| Guest (0003) | FreeBSD 15.x arm64, ZFS, `podman-suite` + `ocijail`, `bastille`, `pf`, `linux` compat | 15.x kernel needed to run 15.x userlands in jails/containers |
| Seed format (0003) | NoCloud ISO, label `cidata`, `#!/bin/sh` user-data | Consumed by `nuageinit` in the official `BASIC-CLOUDINIT` images |
| Network provider (0004) | gvproxy (gvisor-tap-vsock) via QEMU `stream` netdev; `-ssh-port`, expose API; host `podman.sock` via our own `ssh -N -L` helper (gvproxy’s `-forward-sock` kills gvproxy when guest sshd is slow to answer) | 1:1 with `podman machine`; ships with the Homebrew podman formula |
| Port forwarder (0004) | `jm _forwarder <name>`, detached by `jm start` (pid/log/`forwards.json` in the machine dir); desired state from `podman --connection <name> ps --format json`, triggers from `podman events` (debounced 300 ms) plus a 30 s timer; converges gvproxy’s `/services/forwarder/{expose,unexpose,all}` touching only mappings it owns | No guest agent; host port conflicts are per-mapping errors in `jm ports`/`inspect`, retried each resync |
| Control channel (0001) | OpenSSH / `x/crypto/ssh`, root + dedicated ed25519 key | Unprivileged user is an open question |
| Engine API in guest | `podman system service` on `/var/run/podman/podman.sock` via our `podman_service` rc script | Ports' `podman` rc script only restarts containers |
| Packaging | goreleaser + Homebrew tap | |

Known kernel-side limits (document, don't fight): no virtiofs, no vsock.
