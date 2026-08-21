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
jpodman run --rm --os=linux docker.io/alpine echo hi
```

`jm start` registers a `podman system connection` named after the machine but
leaves your default connection alone; `jpodman` (a symlink to `jm`) is
`podman` pointed at that connection, and starts the machine if it is stopped.
The guest is FreeBSD, so podman pulls FreeBSD variants by default: **Linux
images need `--os=linux`** (or `podman pull --os=linux`); native FreeBSD
images need nothing:

```bash
jpodman run --rm docker.io/dougrabson/freebsd15-minimal uname -srm
jm ssh -- bastille list          # jails live in the same VM
```

### Docker CLI and compose

`jm env` prints `DOCKER_HOST` / `CONTAINER_HOST` exports pointing at the
socket gvproxy proxies onto the host:

```bash
eval "$(jm env)"            # fish: eval (jm env --shell fish)
docker ps
```

### Sharing host directories

Host directories appear in the guest **at the same absolute path**, so a
volume argument written on the Mac resolves inside the VM unchanged — from
any directory, with no rewriting and no `/host_mnt` prefix:

```bash
jpodman run --rm --os=linux -v ~/code:/app docker.io/alpine ls /app
jpodman run --rm --os=linux -v ~/code:$HOME/code docker.io/alpine ls $HOME/code
```

A new machine shares your home directory, `/Volumes`, `/private/tmp` and the
per-user temporary directory `$TMPDIR` lives in (`/var/folders/<hash>`), so
`-v $(mktemp -d):/work` and `-v $TMPDIR/x:/work` both work.

> **`/tmp` is the one path that cannot follow the rule.** On macOS it is a
> symlink to `/private/tmp`, and a share mounted at the guest's own `/tmp`
> would shadow it — so jm shares `/private/tmp` instead and never rewrites
> your `-v` argument. Write `-v /private/tmp/x:/app`, not `-v /tmp/x:/app`;
> the latter silently binds the guest's own empty `/tmp/x`. `jm doctor`
> lists the shared roots, so you can see what is covered.

Change the set at `init` time or later:

```bash
jm init --mount /work --mount /srv/data:ro   # on top of the defaults
jm init --no-mounts                          # share nothing
jm set --mount /work --unmount /Volumes      # machine stopped; takes effect on start
jm inspect | grep Share
```

> Shares are for source trees and data. Ownership follows the Mac user,
> `utimes` is a no-op and `chown`/`mkfifo` fail, and they are far slower
> than the guest's ZFS — keep build output in an engine-managed volume.

### Name resolution

Whatever resolves on the Mac resolves in the guest and in containers, with
the same answer: `jm start` runs a small resolver on the host and points the
guest at it, so queries go through macOS's own resolution API. That covers
split-horizon VPN resolvers, per-domain nameservers and search domains from
`scutil --dns`, `/etc/hosts` entries, `.local` mDNS names — and the special-use
development TLDs `.test`, `.invalid`, `.home.arpa` and `.onion`, which a DNS
resolver would otherwise answer NXDOMAIN on its own.

The Mac itself is `host.docker.internal` and `host.containers.internal` from
inside a container, and answers to its own hostname and `.local` name; a host
address of `127.0.0.1` is rewritten to the address that means "the host" in
the guest, so a service on your loopback is reachable.

```bash
jpodman run --rm --os=linux docker.io/alpine ping -c1 host.docker.internal
jm doctor            # asserts the guest resolves a host-only name to the right address
```

> If the guest's resolver cannot be brought up, `jm start` warns and carries
> on with the resolution the guest already had rather than failing; the check
> in `jm doctor` is what reports the loss.

### Publishing ports

`jpodman run -p` works as on any other machine: a forwarder started by
`jm start` watches podman events and maps the ports through gvproxy.

```bash
jpodman run -d --os=linux -p 8080:80 docker.io/busybox httpd -f -p 80
curl http://localhost:8080/
jm ports                    # what is mapped, where it binds, and why something is not
```

> **Published ports bind every interface by default**, as `docker run -p`
> does on Linux: `127.0.0.1`, `::1`, `localhost` **and your LAN address**, so
> anyone on your network can reach the container. Confine them to the
> loopback with `jm init --publish-addr 127.0.0.1` or, on an existing
> machine, `jm set --publish-addr 127.0.0.1` (it applies from the next
> `jm stop` + `jm start`). The address is stored on the machine and shown by
> `jm inspect` and `jm ports`; `JM_PUBLISH_ADDR` is an override read at
> `jm start` time and written onto the record.

Naming a host address in the flag itself (`-p 127.0.0.1:8080:80`) does not
work: the engine binds that address inside the guest, and `jm ports` says so.
Publish it as `-p 8080:80` and choose the host-side address with
`--publish-addr` instead — the `-p` on its own would put it on the LAN.

> Use `busybox httpd`, Caddy or a native FreeBSD image for web servers —
> not `docker.io/nginx` (see known limits).

## Commands

| Command | Does |
|---|---|
| `jm init [name]` | Create a machine: SSH key, image download + SHA256 check, grow disk, NoCloud seed. `--cpus`, `--memory`, `--disk`, `--image`, `--ssh-port`, `--mount`, `--no-mounts`, `--publish-addr` |
| `jm start [name]` | Boot, provision on first boot, wait for the ready marker, connect podman, start the port forwarder. Idempotent |
| `jm stop [name]` | ACPI power-off via QMP, then terminate if the guest ignores it |
| `jm ssh [name] [-- cmd]` | Root shell (or a command) in the guest |
| `jm env [name]` | Shell exports for podman/docker clients |
| `jm ports [name]` | Container ports published on the host, with errors per mapping |
| `jm list` / `jm inspect [name]` | Machines and their computed state (`--json` available) |
| `jm set [name]` | Change `--cpus`, `--memory`, `--disk` (disk grows live), `--mount`/`--unmount` (needs a restart), `--publish-addr` |
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

## Using `jpodman` / `jdocker` instead of touching your podman setup

`jm start` registers a podman connection named after the machine but does
**not** change your default connection. Use the `jpodman` wrapper (a symlink
to `jm`) exactly like `podman`:

```bash
jpodman run --rm --os=linux docker.io/alpine echo hi
jpodman build -t myapp .            # Containerfile, native FreeBSD or Linux base
JM_MACHINE=dev jpodman ps           # pick another machine
jm start --set-default              # opt in: make plain 'podman' use the machine
```

`jdocker` is the same for the docker CLI (`brew install docker` for the
client; jm is the engine), with `DOCKER_HOST` pointed at the machine and
your docker contexts left alone:

```bash
jdocker run --rm docker.io/alpine echo hi
jdocker compose up -d
```

The docker CLI has no `--os` flag, and the guest engine is FreeBSD, so the
wrapper defaults `DOCKER_DEFAULT_PLATFORM` to `linux/arm64` — a plain
`jdocker run` pulls the Linux image, as it would on Docker Desktop. Export
`DOCKER_DEFAULT_PLATFORM` yourself (or pass `--platform`) to build or run
native FreeBSD images through `jdocker`.

Both wrappers **start the machine if it is not running**, printing one line
while it boots; `JM_AUTOSTART=0`, or `--no-autostart` as the first argument,
makes them fail instead.

Published ports behave as they do on Linux; see
[Publishing ports](#publishing-ports) for the default binding and how to
confine it to the loopback.

### Building native FreeBSD images

Use the FreeBSD project's images as a base; `freebsd15-minimal` is for static
binaries and has no `pkg` runtime libraries:

```Dockerfile
FROM ghcr.io/freebsd/freebsd-runtime:15.1
RUN env ASSUME_ALWAYS_YES=yes pkg bootstrap && pkg install -y curl && pkg clean -ay
COPY hello.sh /usr/local/bin/hello
CMD ["/usr/local/bin/hello"]
```

## Jails

`bastille` is installed and configured (ZFS, `bastille0` loopback, NAT via pf):

```bash
jm ssh -- bastille bootstrap 15.1-RELEASE
jm ssh -- bastille create demo 15.1-RELEASE 10.17.89.10
jm ssh -- bastille cmd demo pkg install -y curl
```

Jail management from the host (`jm jail ...`) is post-MVP (ADR 0006).

## Known limits

FreeBSD kernel, not ours — documented, not fought:

- **No virtiofs driver** → host directories are shared over 9p instead.
  It works (see above) but it is slow — tens of MB/s, and roughly 20×
  slower than ZFS on metadata — and `utimes` is silently ignored while
  `chown` and `mkfifo` fail. Keep build output in an engine-managed volume.
- **No vsock driver** → the podman socket is forwarded over SSH.
- **Linuxulator gaps**: Linux AIO (`io_setup`) is missing, so `docker.io/nginx`
  workers die (`epoll`/AIO errors) while the master keeps accepting
  connections that never answer. `busybox httpd`, alpine, Caddy, Python and
  most CLI tools are fine. Native FreeBSD images have no such limits.
- **Ports bound to a loopback host IP** (`-p 127.0.0.1:8080:80`) bind the
  guest's loopback; the forwarder warns in `forwarder.log` and `jm ports`
  reports them instead of forwarding. Use `-p 8080:80` with
  `jm set --publish-addr 127.0.0.1`.
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
