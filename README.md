# jailmachine

**Status: MVP — macOS on Apple Silicon only.** Linux (KVM) and Windows
(Hyper-V) backends are planned; the Linux binaries ship build-only.

`jailmachine` (`jm`) is `docker-machine` / `podman machine` for FreeBSD: one
command brings up a FreeBSD VM that runs **jails** (via `bastille`) and **OCI
containers** (via `podman` — native FreeBSD images, plus Linux images through
the Linuxulator), then points your host's `podman` (or `docker`) client at it.

## Quickstart

```bash
brew install gabrielbelli/tap/jailmachine   # pulls in qemu and podman

jm init      # download the prebaked FreeBSD 15.1 guest image, write the seed, make keys
jm start     # first boot in seconds with the default image (≈2 min with --image official), later ≈12 s
podman run --rm --os=linux docker.io/alpine echo hi
```

`jm start` registers (and makes default) a `podman system connection` named
`jailmachine`, so the host `podman` talks to the VM. The guest is FreeBSD, so
podman pulls FreeBSD variants by default: **Linux images need `--os=linux`**
(or `podman pull --os=linux`); native FreeBSD images need nothing:

```bash
podman run --rm docker.io/dougrabson/freebsd15-minimal uname -srm
jm ssh -- bastille list          # jails live in the same VM
```

### Docker CLI and compose

`jm env` prints `DOCKER_HOST` / `CONTAINER_HOST` exports pointing at the
socket gvproxy proxies onto the host:

```bash
eval "$(jm env)"            # fish: eval (jm env --shell fish)
docker ps
```

### Publishing ports

`podman run -p` works as on any other machine: a forwarder started by
`jm start` watches podman events and maps the ports through gvproxy.

```bash
podman run -d --os=linux -p 8080:80 docker.io/busybox httpd -f -p 80
curl http://localhost:8080/
jm ports                    # what is mapped, and why something is not
```

> Use `busybox httpd`, Caddy or a native FreeBSD image for web servers —
> not `docker.io/nginx` (see known limits).

## Commands

| Command | Does |
|---|---|
| `jm init [name]` | Create a machine: SSH key, image download + SHA256 check, grow disk, NoCloud seed. `--cpus`, `--memory`, `--disk`, `--image`, `--ssh-port` |
| `jm start [name]` | Boot, provision on first boot, wait for the ready marker, connect podman, start the port forwarder. Idempotent |
| `jm stop [name]` | ACPI power-off via QMP, then terminate if the guest ignores it |
| `jm ssh [name] [-- cmd]` | Root shell (or a command) in the guest |
| `jm env [name]` | Shell exports for podman/docker clients |
| `jm ports [name]` | Container ports published on the host, with errors per mapping |
| `jm list` / `jm inspect [name]` | Machines and their computed state (`--json` available) |
| `jm set [name]` | Change `--cpus`, `--memory`, `--disk` (disk grows live) |
| `jm console [name]` | Serial console log (`-f` to follow) |
| `jm rm [name]` | Remove the machine, its directory and the podman connection |
| `jm doctor` | Check qemu, HVF, EDK2 firmware, gvproxy, podman, ssh, the state root and every machine |
| `jm version` / `jm --version` | Build identity (`--json` available) |

`[name]` defaults to `jailmachine`. Exit codes: 0 ok, 1 failure, 2 usage.

## Image sources

| `--image` | What you get | Verification |
|---|---|---|
| `prebaked` (default) | Already-provisioned disk from the `guest-<ver>` GitHub release, built by running `guest/provision.sh` (`jm image build`); first boot in seconds | `.sha256` sidecar next to the image |
| `official` | FreeBSD `BASIC-CLOUDINIT-zfs.raw.xz` from download.freebsd.org, provisioned on first boot by the same script (≈2 min) | `CHECKSUM.SHA256` from the release directory |
| `official:<release>` | Same, pinned (e.g. `official:15.1-RELEASE`) | As above |
| `<path or https URL to .raw\|.raw.xz\|.raw.zst>` | Your own raw disk satisfying the guest contract; `jm` applies the seed, you own provisioning | Sibling `.sha256` if present; otherwise untrusted (`inspect` shows `image_trusted=false`) |

The guest contract is in [ADR 0003](docs/adr/0003-guest-contract-and-image-sources.md) and
[`guest/README.md`](guest/README.md). `guest/provision.sh` is the single
source of truth; prebaked images are produced by running it.

## How it works

QEMU (`qemu-system-aarch64 -M virt,accel=hvf`, EDK2 firmware, virtio) boots a
FreeBSD 15.x arm64 guest. Networking is
[gvproxy](https://github.com/containers/gvisor-tap-vsock) (the same userspace
stack `podman machine` uses, no slirp), which gives the guest `192.168.127.2`,
forwards SSH to a loopback port and proxies the podman socket onto the host.
Host ↔ guest is SSH only, as root with a dedicated ed25519 key. Architecture
decisions are in [`docs/adr/`](docs/adr/), concrete tool choices in
[`docs/tech-choices.md`](docs/tech-choices.md).

State lives in `~/.jailmachine/machines/<name>/` (`disk.raw`, `seed.iso`,
`efivars.fd`, `ssh/`, `machine.json`, logs and sockets); `rm -rf` of that
directory is a complete uninstall. `JM_HOME` or `--state-root` move it.

## Known limits

FreeBSD kernel, not ours — documented, not fought:

- **No virtiofs driver** → no host bind-mounts. Volumes live in the VM; use
  NFS or sshfs for host files.
- **No vsock driver** → the podman socket is forwarded over SSH.
- **Linuxulator gaps**: Linux AIO (`io_setup`) is missing, so `docker.io/nginx`
  workers die (`epoll`/AIO errors) while the master keeps accepting
  connections that never answer. `busybox httpd`, alpine, Caddy, Python and
  most CLI tools are fine. Native FreeBSD images have no such limits.
- **Ports bound to a loopback host IP** (`-p 127.0.0.1:8080:80`) bind the
  guest's loopback; `jm ports` reports them instead of forwarding.
- **No routable VM IP** yet (gvproxy is NAT); vmnet/bridged is a later step.
- **Apple Virtualization.framework / vfkit** cannot boot FreeBSD/arm64 (the
  kernel dies after `No valid device tree blob found!`), hence QEMU.

## Troubleshooting

| Symptom | Do |
|---|---|
| Anything at all | `jm doctor` — checks every tool and machine and prints a fix per failure |
| `start` hangs or fails at a stage | The error names the stage and the log to read; `jm console` shows the guest's serial console (`-f` to follow the boot) |
| Provisioning failed | `jm ssh -- cat /var/log/jm-provision.log`; the marker `/var/db/jm-provision-failed` means the script aborted |
| Port not reachable | `jm ports` lists each mapping with its error (host port busy, loopback bind, forwarder down) |
| Stale state after a crash or reboot | `jm stop` repairs "broken" (pid file without process); `jm rm && jm init` is always a clean slate |

Host-side logs, all under `~/.jailmachine/machines/<name>/`:

| File | From |
|---|---|
| `console.log` | guest serial console (`jm console`) |
| `qemu.log` | QEMU's own stdout/stderr |
| `gvproxy.log` | network provider |
| `forwarder.log` | port-publishing loop |

## Building from source

```bash
git clone https://github.com/gabrielbelli/jailmachine && cd jailmachine
make build && ./jm version
make test lint
JM_E2E=1 make e2e     # full init → start → podman run → stop → rm, needs qemu + podman
```

`bin/jm` is the original shell proof of concept, kept as legacy reference;
the Go binary is the product.

## Licence

BSD-2-Clause.
