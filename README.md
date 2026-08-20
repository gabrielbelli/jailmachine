# jailmachine

A `docker-machine`-style experience for FreeBSD: one command brings up a
lightweight FreeBSD VM that runs **jails** (via `bastille`) and **OCI
containers** (via `podman`, native FreeBSD images and Linux images through
the Linuxulator), then wires your host's `podman` client to it.

```
jm init      # download image, create the VM
jm start     # boot (≈3 s on Apple Silicon)
jm ssh       # shell into it
jm stop
jm rm
```

## Status

Early. macOS (Apple Silicon) first, on Apple's Virtualization.framework via
[vfkit](https://github.com/crc-org/vfkit). Linux (KVM) and Windows (Hyper-V /
WSL2) backends are planned.

## Why

`podman machine` is hard-wired to Fedora CoreOS. If you want a FreeBSD
userland — jails, pf, ZFS, native FreeBSD OCI images — there is no
one-command equivalent. This is that.

## Known limits (FreeBSD kernel, not ours)

- No virtiofs driver → no host bind-mounts; volumes live in the VM (NFS/sshfs
  for host files).
- No vsock driver → the podman socket is forwarded over SSH.

## Licence

BSD-2-Clause.
