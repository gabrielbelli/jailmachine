# Troubleshooting

> **This release is an MVP — a working demo.** It proves the whole idea end
> to end and is usable day to day, but the polished behaviour (host
> directory mounts at identical paths, DNS 1:1 with the host, autostart,
> `docker` CLI parity) is being built right now on the `docker-parity`
> branch and is **not** in this release. Several entries below are limits of
> this release rather than faults to fix.

Everything here is on macOS/Apple Silicon, the only supported host. For how
the pieces fit together, see [ARCHITECTURE.md](ARCHITECTURE.md).

## Four commands that answer most questions

```bash
jm doctor                 # host tools, state root, every machine; exits 1 if any check fails
jm inspect --json         # the record plus live state; nothing is cached
jm console -n 50          # the guest's serial console (-f follows the boot)
jm ssh -- <command>       # run anything in the guest, e.g. jm ssh -- service podman_service status
```

`jm doctor` checks `qemu-system-aarch64` (≥ 8) and HVF, the EDK2 firmware,
`gvproxy`, `podman` (≥ 5), `ssh`/`ssh-keygen`, `xz`, the state root, the
socket-path budget, and each machine's combined state — printing a one-line
fix per failure. Note that it inspects the default state root
(`~/.jailmachine`, or `$JM_HOME`) unless you pass `--state-root`.

## Symptom → cause → fix

| Symptom | Cause | Fix |
|---|---|---|
| `podman ps` → `connection refused` / `dial unix … no such file` | Machine stopped, or the default connection points elsewhere | `jm start`; or use `jpodman ps`; or `jm start --set-default` |
| `podman` hits the wrong machine | Another `podman system connection` is the default | `JM_MACHINE=<name> jpodman ps`, or `podman --connection <name> …` |
| `qemu-system-aarch64 not found on PATH` | QEMU not installed | `brew install qemu` (the Homebrew cask declares it as a dependency) |
| `gvproxy: not found on PATH` | gvproxy missing | `brew install podman` (ships gvproxy), or set `$JM_GVPROXY` |
| `jm start` → `network: …` | gvproxy exited | Read `gvproxy.log`; see *gvproxy dying* below |
| `jm start` → `backend: qemu: failed to start` | QEMU, firmware or HVF problem | `jm doctor`; read `qemu.log` |
| `jm start` → `ssh: timed out` | Guest is not booting | `jm console -n 100`; see *EFI shell* below |
| `jm start` → `provision: provisioning script failed in the guest` | `provision.sh` aborted (usually `pkg`) | `jm ssh -- tail -50 /var/log/jm-provision.log`, then `jm rm && jm init` |
| `jm start` → `provision: timed out waiting for provisioning` | Official-image first boot still installing, or DNS is down in the guest | `jm ssh -- tail -f /var/log/jm-provision.log` |
| `jm doctor` → `machine <name> … broken` | Half of the machine is up, or a stale pid file | `jm stop <name>`, then `jm start <name>` |
| `jm <cmd>` → `another jm command is operating on "<name>"` | Per-machine advisory lock is held | Wait for the other command; it is released when that process exits |
| Published port not reachable | Host port busy, loopback bind, or the forwarder is down | `jm ports` prints the reason per mapping |
| `docker.io/nginx` workers die with `epoll_ctl(1, 6) failed (22: Invalid argument)` | FreeBSD's `linux_epoll` has no `EPOLLEXCLUSIVE`, which nginx uses when `worker_processes > 1` | Add `accept_mutex on;` to the `events` block (or `worker_processes 1;`), or run `ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator` |
| `redis-server` exits with `Failed to test the kernel for a bug … Redis will now exit` | Redis' ARM64 copy-on-write probe cannot run under the Linuxulator | `redis-server --ignore-warnings ARM64-COW-BUG` |
| `docker.io/node` prints `node --version` but nothing from `console.log`, and its servers never accept | Known-bad Linux image in this release | None. Use another image; see [USAGE](USAGE.md#node-the-one-known-bad-image) |
| `docker compose up` → `no image found in image index for … OS "freebsd"` | Compose cannot ask for a platform, and the guest is FreeBSD | `jpodman pull --os=linux <image>` first, then `pull_policy: missing` on the service |
| `-v /Users/me/src:/src` does nothing useful | No host directory sharing in this release | See *no host directory sharing* below |
| `no space left on device` in the guest | Guest disk full | `jm set --disk <bigger>` |
| `jm doctor` warns on `socket paths` | `--state-root` so deep that sockets no longer fit in `sun_path` (103 bytes) and fall back to `$TMPDIR` | Harmless, but a shorter state root keeps every file in one directory |
| `jm rm` refuses because the machine will not stop | The hypervisor is wedged | `jm rm --force <name>` |
| `jm <cmd>` → `no machine named "jailmachine" and several exist` | Ambiguous default | Name one: `jm start dev`. For `jm podman`/`jpodman` the hint's `jm podman <name>` form does **not** work — that command takes no name; use `JM_MACHINE=dev jpodman ps` |

## `connection refused` from podman

`jm start` registers two connections and, unless you pass `--set-default`,
**does not repoint a default you already had** (if you had no podman
connections at all, podman itself promotes the first one jm registers):

| Connection | URI |
|---|---|
| `<name>` | `ssh://root@127.0.0.1:<ssh-port>/var/run/podman/podman.sock` |
| `<name>-sock` | `unix://<machine dir>/podman.sock` (the `ssh -N -L` forward) |

So plain `podman` may well be talking to something else. See what exists:

```bash
podman system connection ls
```

Then pick one of these, rather than all of them:

| Want | Command |
|---|---|
| Reach the machine without touching your default | `jpodman ps` |
| Reach a specific machine | `JM_MACHINE=dev jpodman ps` |
| Make plain `podman` use the machine from now on | `jm start --set-default` |
| Point `docker` / compose at it | `eval "$(jm env)"` (fish: `eval (jm env --shell fish)`) |

If the connection is right and it still refuses, the machine is not running
or the API forward died:

```bash
jm inspect --json | jq -r '.state, .api_socket'
jm ssh -- service podman_service status
tail -20 ~/.jailmachine/machines/jailmachine/forward.log
jm start                         # idempotent: re-does ssh, provision, connect, forwarder
```

## Machine stuck in `broken`

`broken` means the hypervisor and the network provider disagree — QEMU is
running with gvproxy dead, gvproxy is running with no QEMU, or a pid file
outlived its process (host reboot, `Ctrl-C` mid-`start`). It is a diagnosed,
recoverable condition, not corruption.

```bash
jm inspect --json | jq -r '.state, .backend_state, .network_state'
jm stop      # converges both halves to stopped (asks the guest first if it is alive)
jm start
```

`jm start` on a broken machine does the same repair itself. If `stop` cannot
finish, `jm stop --force` terminates the hypervisor outright, and
`jm rm --force` always converges to "gone".

## Boot drops to the EFI shell

The EDK2 variable store (`efivars.fd`) is per machine and can be left
inconsistent by a hard kill during boot. The symptom is a `Shell>` prompt in
`jm console` and an `ssh` stage that times out. Delete the store; `jm start`
copies a pristine one from the QEMU firmware directory:

```bash
jm stop
rm ~/.jailmachine/machines/jailmachine/efivars.fd
jm start
```

To watch the boot as it happens, run `jm console -f` in a second terminal.

A store whose size differs from the firmware template (a truncated copy, or
a template from a different QEMU build) is treated as absent and replaced
automatically — worth knowing after a `brew upgrade qemu`.

## First boot slow, or failing on `pkg`

Which path you are on decides what "slow" means:

| Image | First boot | Why |
|---|---|---|
| `prebaked` (default) | about 22 s | Disk is already provisioned; the seed only applies the key and hostname |
| `official[:<release>]` | about 2 min | `provision.sh` runs in full: `pkg install podman-suite bastille`, ZFS dataset, Linuxulator, `pf`, `podman_service` |
| any, later starts | 12–20 s | Nothing to provision — the time is the guest's own boot |

A busy host stretches all three: 32 s was measured for a prebaked first boot
with two other VMs already running. `jm init` is separate and takes about
45–60 s, almost all of it the roughly 800 MiB download of the
`guest-15.1.0` release asset and its SHA256 check.

The `provision` stage waits up to 15 minutes and fails fast if
`/var/db/jm-provision-failed` appears. Watch or read the log in the guest:

```bash
jm ssh -- tail -f /var/log/jm-provision.log
jm ssh -- 'ls -l /var/db/jm-provision* ; freebsd-version -k; freebsd-version -r'
```

Common causes on the official path: no DNS yet in the guest (`pkg` cannot
resolve `pkg.freebsd.org`), or a mirror timing out. The failure marker is
terminal for that disk — `provision.sh` only runs once per disk life — so
recover with `jm rm && jm init`, optionally pinning a release:

```bash
jm rm && jm init --image official:15.1-RELEASE && jm start
```

If the official image installed a new kernel during its own first boot,
`jm start` reboots the guest once (`freebsd-version -k` ≠ `-r`) before
connecting podman, so the Linuxulator modules load on the kernel they were
built for. One extra reboot in the `provision` stage is expected, not a
fault.

## gvproxy dying

gvproxy is the whole network: SSH, DNS, the podman socket forward and every
published port. If it exits, the machine reads as `broken` and the running
guest loses its NIC.

```bash
tail -40 ~/.jailmachine/machines/jailmachine/gvproxy.log
tail -40 ~/.jailmachine/machines/jailmachine/forward.log
jm stop && jm start
```

`jm` deliberately does not use gvproxy's own `-forward-sock`: gvproxy 0.8.x
exits altogether when guest `sshd` is slow to answer, which a FreeBSD first
boot routinely is. The host `podman.sock` is served by a separate detached
`ssh -N -L` helper instead, logged to `forward.log`, so a dead socket
forward no longer takes networking down with it — restart it with
`jm start`.

## Port not reachable

Ports are published by reconciliation: a detached forwarder watches
`podman ps`/`podman events` and converges gvproxy's mapping table. Ask it
what it did:

```bash
jm ports
```

```text
LOCAL           REMOTE              PROTO  STATUS
127.0.0.1:8080  192.168.127.2:8080  tcp    ok
```

| `jm ports` says | Meaning | Fix |
|---|---|---|
| `# port forwarder for <name> is not running` | The loop is down | `jm start` (idempotent) |
| `error: …address already in use` | Another process holds the host port | Free it, or republish on a different host port; it is retried at the next resync (30 s) |
| `REMOTE` is `-`, `STATUS` is `error: guest binds 127.0.0.1 only…` | `-p 127.0.0.1:8080:80` bound the **guest's** loopback, so nothing on the host can reach it | Publish without a host IP: `-p 8080:80` |
| nothing at all | The container is not running, or publishes no ports | `jpodman ps` |

Mappings a container should have but the table lacks usually mean the
forwarder cannot reach the engine; `forwarder.log` records every `podman ps`
error. The forwarder only ever removes mappings it owns (tracked in
`forwards.json`), so a restart rebuilds rather than loses the table.

## Linux images under the Linuxulator

Linux images need `--os=linux` on the host podman (or `podman pull
--os=linux`); native FreeBSD images need nothing. Nearly everything on
Docker Hub works — the verified matrix is in
[USAGE.md](USAGE.md#docker-hub-compatibility-verified). Three cases need
knowing about.

### nginx: workers die at startup

```
[alert] 8231#8231: epoll_ctl(1, 6) failed (22: Invalid argument)
[alert] 8154#8154: worker process 8231 exited with fatal code 2 and cannot be respawned
```

Stock `docker.io/nginx` fails as shipped, and **the cause is not AIO**.
With `worker_processes > 1` nginx registers its listening socket with
`EPOLLEXCLUSIVE`; FreeBSD's `linux_epoll` does not implement that flag and
returns `EINVAL`, which nginx treats as fatal. One line in the `events`
block fixes it:

```nginx
events {
    accept_mutex on;
    worker_connections 1024;
}
```

`worker_processes 1;` or `reuseport` on the `listen` directive work too.
Measured here: 4 workers with `accept_mutex off` → workers die; 4 workers
with `accept_mutex on` → HTTP 200 with all four alive. A ready-made image is
published:

```bash
jpodman run -d --os=linux -p 8080:80 ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator
curl --retry 10 --retry-connrefused http://localhost:8080/healthz    # ok
```

The separate line `io_setup() failed (38: Function not implemented)` is a
**harmless one-off**: nginx probes for Linux AIO, does not find it, disables
file AIO and serves normally. It is not the failure and cannot be silenced.
There is no measurable speed penalty either — 1000 sequential requests took
1.55 s under the Linuxulator against 1.56 s for native FreeBSD nginx.

### redis: refuses to start

```
Failed to test the kernel for a bug that could lead to data corruption
during background save … Redis will now exit.
```

Redis' own switch skips the probe:

```bash
jpodman run -d --os=linux -p 6379:6379 docker.io/library/redis:alpine \
    redis-server --ignore-warnings ARM64-COW-BUG
printf 'PING\r\n' | nc 127.0.0.1 6379      # +PONG
```

### node: no workaround

`docker.io/library/node:22-alpine` is the one image in the matrix that does
not work. `node --version` prints `v22.23.2`, so the binary starts, but
`console.log` output never reaches the pipe and an HTTP server started
inside the container never accepts a connection (`connection reset by peer`,
locally and through a published port). `UV_USE_IO_URING=0` makes no
difference. Use another image.

### General

Check what a misbehaving Linux container is actually doing with
`jpodman logs <container>`, and confirm the Linuxulator is loaded with
`jm ssh -- kldstat | grep linux`. A quick known-good check:

```bash
jpodman run -d --os=linux -p 8080:80 --name web docker.io/busybox \
  sh -c 'echo hello from the FreeBSD VM > /tmp/index.html && httpd -f -p 80 -h /tmp'
curl --retry 10 --retry-connrefused http://localhost:8080/
jpodman rm -f web
```

The retry flags matter: the forwarder reconciles a second or two after the
container starts, so a bare `curl` usually gets one connection refused
first.

## No host directory sharing yet

There is none in this release. FreeBSD has no virtiofs driver, and there is
no vsock either (hence the podman socket over SSH). `-v /host/path:/in/ctr`
will not do what it does on Docker Desktop: the path is resolved **inside
the VM**.

For now, move files explicitly:

```bash
jm ssh -- mkdir -p /root/src
tar cf - -C ~/src . | jm ssh -- tar xf - -C /root/src
jpodman run --rm --os=linux -v /root/src:/src docker.io/busybox ls /src   # a guest path, not a host one
```

9p-based sharing — host directories at identical paths — is what the
`docker-parity` branch is building.

## Disk full, and growing the disk

The guest disk is a sparse `disk.raw` grown to `--disk` at `init` (64 GiB by
default); ZFS claims the space on first boot. Check and grow:

```bash
jm ssh -- 'zpool list zroot; df -h /'
```

```bash
jm set --disk 128     # grow only; jm never shrinks a disk
```

On a **running** machine the growth is applied live: QEMU is told the file
grew (QMP `block_resize`), then `gpart resize` and `zpool online -e` run in
the guest, and the guest verifies the presented size first, so `set` cannot
report success while the pool is unchanged. On a **stopped** machine the
grow is recorded and applied at the next `jm start`. `--cpus`, `--memory`
and `--ssh-port` need the machine stopped.

Host-side, the image stays sparse; `du -h ~/.jailmachine/machines/<name>/`
shows what it really occupies. Removing images inside the guest
(`jpodman image prune -a`) frees guest space but does not shrink
`disk.raw`.

## Where every log lives

Host-side, under `~/.jailmachine/machines/<name>/` (or `$JM_HOME`, or
`--state-root`):

| File | Written by | Read it when |
|---|---|---|
| `console.log` | the guest's serial console | The guest will not boot (`jm console`, `-f` to follow) |
| `qemu.log` | QEMU's own stdout/stderr | The `backend` stage fails |
| `gvproxy.log` | gvproxy | The `network` stage fails, or networking dies under a running guest |
| `forward.log` | the `ssh -N -L` helper serving `podman.sock` | `unix://…/podman.sock` refuses connections |
| `forwarder.log` | the port-publishing loop | A published port never appears |
| `forwards.json` | the same loop | You want the owned mapping table without running anything |

In the guest:

| Path | Contents |
|---|---|
| `/var/log/jm-provision.log` | Everything `provision.sh` did, both paths |
| `/var/db/jm-provisioned` | Ready marker — provisioning finished |
| `/var/db/jm-provision-failed` | Failure marker — the script aborted |
| `/var/log/podman_service.log` | The `podman system service` rc script |
| `/var/run/podman/podman.sock` | The engine API the host connects to |

```bash
jm inspect                    # prints console, network and forwarder log paths
jm ssh -- tail -50 /var/log/jm-provision.log
```

## Starting over

`rm -rf` of a machine's directory is a valid, complete uninstall, but `jm rm`
also stops the machine, forgets its podman connections and its host key, and
cleans up any socket that lives in `$TMPDIR`:

```bash
jm rm                 # or: jm rm --force dev
jm init && jm start
```
