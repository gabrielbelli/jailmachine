# Troubleshooting

> **Still an MVP — a working demo**, but a wider one than v0.1.0: host
> directory mounts at identical paths, name resolution 1:1 with the host,
> autostart on demand, the `jdocker` wrapper and docker-identical `-p`
> semantics are all here, and so is UDP — publishing included. Several
> entries below are deliberate limits rather than faults; the one real
> Linuxulator gap left is narrow enough to have its own section
> ([UDP in a Linux container](#udp-in-a-linux-container)).

Everything here is on macOS/Apple Silicon, the only supported host. For how
the pieces fit together, see [ARCHITECTURE.md](ARCHITECTURE.md); for the
limits that are not bugs — measured, and attributed to the layer that owns
them — see [LIMITATIONS.md](LIMITATIONS.md) and, against Docker Desktop and
podman machine, [COMPARISON.md](COMPARISON.md).

## Four commands that answer most questions

```bash
jm doctor                 # host tools, state root, every machine; exits 1 if any check fails
jm inspect --json         # the record plus live state; nothing is cached
jm console -n 50          # the guest's serial console (-f follows the boot)
jm ssh -- <command>       # run anything in the guest, e.g. jm ssh -- service podman_service status
```

`jm doctor` checks `qemu-system-aarch64` (≥ 8) and HVF, the EDK2 firmware,
`gvproxy`, `podman` (≥ 5), `ssh`/`ssh-keygen`, `xz`, the state root, the
socket-path budget, the `jpodman`/`jdocker` wrappers and autostart, then per
machine: its combined state, **share parity** (a file written on the host
must be visible to a container at the same path), **resolution parity** (a
name only the host can resolve must resolve in the guest, to the same
address), the published-UDP **datagram limit** and the guest **clock** —
printing a one-line fix per failure. That is 23 checks on a Mac with one
machine. Note that it inspects the default state root
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
| `docker.io/node` on alpine prints `node --version` and then hangs on everything else | FreeBSD's `linux_mremap` cannot grow a mapping, and musl's allocator retries forever | Use `node:22-bookworm-slim` (glibc); see [USAGE](USAGE.md#node-the-one-known-bad-image) |
| `docker compose up` → `no image found in image index for … OS "freebsd"` | Compose cannot ask for a platform, and the guest is FreeBSD | `jpodman pull --os=linux <image>` first, then `pull_policy: missing` on the service — or `platform: linux/arm64` on it. See [Compose and Kubernetes YAML](USAGE.md#compose-and-kubernetes-yaml) |
| `jpodman compose …` → `command [ssh -l root … docker system dial-stdio] has exited with exit status 255` | podman's compose shim handed the external provider the machine's `ssh://` URI, so Compose went looking for a docker daemon **inside** the guest | Nothing on a current jm: a `compose` invocation is targeted at the machine's socket connection (`<name>-sock`) with `DOCKER_HOST` set to it. On an older binary, use `jdocker compose …`, or `eval "$(jm env)"` then `docker compose …`. See [Compose and Kubernetes YAML](USAGE.md#compose-and-kubernetes-yaml) |
| `jpodman kube play` fails with an error naming `catatonit` | A pod's infra container needs `catatonit` as its init, and the guest image predates the package | `jm ssh -- pkg install -y catatonit`, or re-create the machine on a current image (`jm rm && jm init && jm start`), which installs it with `podman-suite` |
| `-v /Users/me/src:/app` mounts an empty directory | The host path is outside the machine's share set, or it is under `/tmp` | `jm inspect` lists the shares; write `/private/tmp/...` not `/tmp/...`; add a root with `jm stop && jm set --mount <dir> && jm start`. See *a share is empty* below |
| `Permission denied` writing inside a shared directory, or `git clone` into one dies with `Unable to create temporary file '….git/objects/pack/tmp_pack_XXXXXX'` | The machine was started with `JM_9P_SECURITY=none`, where the host end of the share acts as your unprivileged Mac user and cannot rewrite a file the container has just made read-only | Drop the override (the default is `mapped-xattr`) and `jm stop && jm start`. See *permission denied writing to a share* below |
| `zsh: no such file or directory` from `jm set --mount $P:ro` | zsh reads a trailing `:ro` as a history modifier | Quote it: `jm set --mount "${P}:ro"`, and `-v "${P}:${P}:ro"` |
| A name resolves on the Mac but not in a container | The host resolver is down, or the guest was not pointed at it | `jm doctor`, then `resolver.log`. See *names do not resolve* below |
| UDP datagrams over 8972 bytes never arrive | The gvproxy link does not fragment, and its MTU is 9000 by default | Keep datagrams under 8972 bytes, or use TCP; `jm doctor` states the limit, and `JM_MTU` at `jm start` moves it. See *UDP in a Linux container* below |
| `nc -u -l` in a Linux container → `Address family not supported by protocol` | busybox's UDP listener peeks its peer with a zero-length `recvmsg()`, which returns at once on FreeBSD where Linux blocks | `apk add netcat-openbsd`, or `socat UDP4-RECVFROM:…`. UDP itself works; see *UDP in a Linux container* below |
| `jpodman ps` pauses and prints `starting jailmachine …` | Autostart is doing its job | Nothing. `JM_AUTOSTART=0` or `jpodman --no-autostart ps` to fail instead |
| `jdocker: the docker CLI is not on PATH` | Only the engine is provided by jm | `brew install docker` (the client alone), or use `jpodman` |
| `no space left on device` in the guest | Guest disk full | `jm set --disk <bigger>` |
| `jm doctor` warns on `socket paths` | `--state-root` so deep that sockets no longer fit in `sun_path` (103 bytes) and fall back to `$TMPDIR` | Harmless, but a shorter state root keeps every file in one directory |
| `jm rm` refuses because the machine will not stop | The hypervisor is wedged | `jm rm --force <name>` |
| `jm <cmd>` → `no machine named "jailmachine" and several exist` | Ambiguous default | Name one: `jm start dev`. `jm podman`/`jm docker` take no name — the error tells you to use `JM_MACHINE=dev jpodman ps` |

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
| any, later starts | 12–25 s | Nothing to provision — the time is the guest's own boot |

A busy host stretches all three: 32 s was measured for a prebaked first boot
with two other VMs already running, and 36.7 s for a warm start on the same
kind of load. `jm init` is separate and takes **60–115 s**: it fetches the
roughly 800 MiB `guest-15.1.0` release asset and checks its SHA256, but the
download is only about 31 s of that — writing `disk.raw` out dominates, which
is a bug of ours
([LIMITATIONS](LIMITATIONS.md#the-machine-itself)).

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
# publishing on 0.0.0.0 unless -p names a host address
LOCAL           REMOTE              PROTO  STATUS
0.0.0.0:8080    192.168.127.2:8080  tcp    ok
127.0.0.1:8082  192.168.127.2:8082  tcp    ok
```

The `#` line names the machine's **default** publish address — `0.0.0.0`, so
a plain `-p 8080:80` is reachable from your whole network. `jm set
--publish-addr 127.0.0.1` (then `jm stop && jm start`) changes that default,
and `-p 127.0.0.1:8080:80` confines a single container to your loopback
without changing anything machine-wide, as it does under Docker Desktop.

| `jm ports` says | Meaning | Fix |
|---|---|---|
| `# port forwarder for <name> is not running` | The loop is down | `jm start` (idempotent) |
| `error: another process on this Mac already holds this host port` | Exactly that; the message carries the `lsof` to run | Free it, or republish on a different host port; it is retried at the next resync (30 s) |
| the same, on a `/udp` mapping to host port 5353 | macOS runs mDNSResponder on `5353/udp` | Publish on another host port — `-p 5354:53/udp`. Nothing about UDP is at fault |
| `error: …cannot assign requested address` | `-p <addr>:…` named an address your Mac does not have | Use one it has (`ifconfig`), or drop the address |
| `error: the container has no address…yet`, `error: installing the redirect in the guest…` | The host side is bound but the guest-side redirect a `-p <addr>:…` needs is not in place yet | Wait one resync (30 s); if it persists, read `forwarder.log`. `jm ssh -- pfctl -a rdr/jm -s nat` lists the rules jm loaded (it errors with `DIOCGETRULES` when there are none, which is not a fault) |
| `error: port N/tcp on the guest already carries container <id>'s publish` | The same guest port is published twice, one of them with a host address; only one can own the guest-side redirect. The id names the holder — often the same container (`-p 8080:80 -p 127.0.0.1:8080:81`), which the message calls `this container's own publish` | Give one of them a different host port |
| `# the record says <addr>; this forwarder keeps <addr>` | `jm set --publish-addr` since the machine started | `jm stop && jm start` to apply it |
| The port answers on `localhost` but you did not expect it on the LAN | A plain `-p` binds `0.0.0.0` by default, as `docker run -p` does on Linux | `-p 127.0.0.1:8080:80` for that container, or `jm set --publish-addr 127.0.0.1` for the machine |
| nothing at all | The container is not running, or publishes no ports | `jpodman ps` |

> Inside the guest, `curl localhost:<published port>` answers nothing even
> when the mapping works: the engine's own port reservation socket wins over
> the redirect for guest-local traffic. Test from the Mac, not from
> `jm ssh`.

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

### node: use the glibc image

`docker.io/library/node:22-alpine` runs `node --version` (`v22.23.2`, exit 0)
and **nothing else**. `node -e ''` and `node -p 1+1` hang before they reach
any script, with or without a TTY; under `truss` the process loops on
`linux_mremap(...) ERR#-12 'Cannot allocate memory'`, one thread spinning at
100 % of a core while four sleep on `futex`. FreeBSD's Linuxulator cannot
grow a mapping, and musl's `mallocng` grows its heap that way.
`UV_USE_IO_URING=0` makes no difference.

**Use `node:22-bookworm-slim`** — the glibc build, verified working including
exit codes. This is not a musl problem in general: alpine's busybox,
`python:3-alpine`, `apk add` and `pip install` all work.

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

## UDP in a Linux container

**UDP works.** Binding, sending, receiving, DNS-over-UDP and publishing with
`-p <host>:<port>/udp` were all verified end to end from a Linux container,
reached from the Mac's loopback and from its LAN address. If you read
somewhere that Linux containers cannot bind UDP sockets, that claim was
wrong and has been withdrawn.

One idiom fails, and it is worth recognising because it is the one most
people reach for when testing UDP by hand:

```
$ jpodman run --rm --os=linux docker.io/alpine sh -c 'nc -u -l -p 9999'
nc: can't connect to remote host: Address family not supported by protocol
```

### Which layer refuses, and why

Not podman, not `ocijail`, not jm's forwarder, and not the address family.
It is FreeBSD's own socket layer, inherited by the Linuxulator.

busybox's `nc -u -l` has to learn who is talking to it before it can reply,
and it does that by peeking the sender's address with a **zero-length**
`recvmsg(…, MSG_PEEK)`, then `connect()`ing the socket to whatever address
came back:

```
socket(AF_INET, SOCK_DGRAM, IPPROTO_IP)                = 3
bind(3, {AF_INET, 0.0.0.0:9999}, 16)                   = 0
recvmsg(3, {msg_namelen=16 => 0, iov_len=0}, MSG_PEEK) = 0
connect(3, {sa_family=AF_UNSPEC, …}, 16)               = -1 EAFNOSUPPORT
```

On Linux that `recvmsg()` **blocks** until a datagram arrives and fills in
`msg_name`. On FreeBSD it returns `0` at once with `msg_namelen = 0`, so
busybox connects to an all-zero sockaddr and gets `EAFNOSUPPORT`. macOS
behaves like FreeBSD here, and so does a C program run natively in the guest
outside any container — this is BSD socket behaviour, not a container or
jail artefact.

The divergence is the **zero-length buffer alone**, not `MSG_PEEK` and not
the address family:

| `recvmsg` on an empty UDP socket | FreeBSD guest | macOS | Linux |
|---|---|---|---|
| 0 bytes, `MSG_PEEK` | returns at once | returns at once | blocks |
| 0 bytes, no flags | returns at once | returns at once | blocks |
| 1 byte, `MSG_PEEK` | blocks | blocks | blocks |
| 1 byte, no flags | blocks | blocks | blocks |

`AF_INET`, `AF_INET6` and `AF_UNIX` datagram sockets all create and bind
normally, so forcing IPv4 with `nc -s 0.0.0.0` does not help: it produces a
`socket(AF_INET, …)` and the identical failure at the same `connect()`.
There is nothing for jm to fix; a remedy would be a Linuxulator change in
FreeBSD itself.

### What to use instead

```bash
# a real netcat
jpodman run --rm --os=linux docker.io/alpine \
  sh -c 'apk add -q netcat-openbsd && nc -u -l -p 9999'

# socat
jpodman run --rm --os=linux docker.io/alpine \
  sh -c 'apk add -q socat && socat UDP4-RECVFROM:9999,fork SYSTEM:"tr a-z A-Z"'

# and publishing, which needs nothing special
jpodman run -d --os=linux -p 5354:53/udp <image>
```

### Large datagrams vanish

If datagrams up to a few kilobytes work and larger ones never arrive, this
is the cause and not a bug in your program. The host-to-guest link is
gvproxy's and **it does not fragment**, so the largest UDP payload that
survives is the link MTU less the 20-byte IPv4 and 8-byte UDP headers. That
MTU is **9000** by default — the virtio-net jumbo frame — which puts the
ceiling at **8972 bytes**. Over that, the datagram is dropped with no error
at either end and nothing in any log.

```
8972 bytes -> reply 8972
8973 bytes -> no reply
```

`jm doctor` states the limit for each machine:

```
[ ok ]  datagram limit dev   published udp carries payloads up to 8972 bytes (gvproxy MTU 9000); larger datagrams are dropped, not fragmented. $JM_MTU changes the link size (576..16384; JM_MTU=1500 matches Docker)
```

`$JM_MTU` moves the ceiling. It is read at `jm start`, clamped to 576–16384,
and the guest takes the value over DHCP — `jm ssh -- ifconfig vtnet0` shows
the `mtu` it settled on, which is the authority: `jm doctor` states the limit
from the variable in *its* environment, not from the running machine.

```bash
JM_MTU=1500 jm start        # Docker's exact link size; ceiling back to 1472
JM_MTU=4000 jm start        # anything in between
```

There is a ceiling either way: gvproxy still never fragments, so this is
**not** parity with Linux, where the sender's stack would split an oversized
datagram and the receiver would reassemble it. The default simply puts the
wall six times further out than a 1500-byte link does.

TCP never meets this, because the stack segments to fit — measured
throughput is the same at 9000 as at 1500. Keep datagrams under the limit,
or use TCP. DNS is unaffected in practice: an oversized reply is truncated
and the resolver retries over TCP, which is what the truncation bit is for.

### If a published UDP port does not answer

Check `jm ports` first. A UDP mapping shows `ok` like any other, and the
usual cause of a failure is that something on the Mac already holds the host
port — `5353/udp` is mDNSResponder's:

```
0.0.0.0:5353  192.168.127.2:5353  udp  error: another process on this Mac already holds this host port (lsof -nP -iUDP:5353); publish the container on a different host port
```

Run the `lsof` the message gives you, then publish on a free host port. The
mapping is retried every resync, so freeing the port is enough.

## A share is empty, or a `-v` mounts nothing

Host directories appear in the guest **at the same absolute path** they have
on the Mac, so `-v /Users/me/src:/app` works from anywhere. When it mounts an
empty directory instead, it is one of four things:

```bash
jm inspect | grep -i share    # what is actually shared
jm doctor                     # writes a host file and asserts a container sees it
```

| Cause | Fix |
|---|---|
| The host path is outside the share set | `jm stop && jm set --mount /that/root && jm start` — the share set changes only on a stopped machine and is attached at the next start. Defaults are your home tree, `/Volumes`, `/private/tmp` and `$TMPDIR`'s parent |
| You wrote `/tmp/...` | Write `/private/tmp/...`. On macOS `/tmp` is a symlink to `/private/tmp`, and a share at the guest's own `/tmp` would shadow it, so jm shares the real path and never rewrites your argument |
| The share shows `— missing on the host, not shared` | The directory has vanished (an unplugged disk). `jm start` drops it with a warning and picks it up again at the next start once it is back |
| The guest image predates the `jm_shares` service | `jm start` warns about it. Re-create the machine on a current image: `jm rm && jm init && jm start` |

A share whose *guest* mount failed is logged rather than fatal — the machine
boots regardless — so `jm doctor` and `jm start`'s warnings are what tell you.

Two more things that look like bugs and are not:

- **`utimes` is a silent no-op** on a 9p share, and guest-side ownership and
  modes live in host xattrs rather than on the host file. A build that sets
  timestamps will misbehave; keep build output in an engine-managed volume
  (`-v myvol:/out`) on the guest's ZFS.
- **An `inotify` watch cannot be created on a share.** It is not that events
  go missing — `inotify_add_watch` fails outright:
  `inotifywait: Couldn't watch /w: Bad file descriptor`. The same image
  watches its own filesystem and an engine-managed volume without trouble, so
  the gap is `p9fs` specifically
  ([#4](https://github.com/gabrielbelli/jailmachine/issues/4)). Reads are
  coherent immediately, so a polling watcher works —
  `CHOKIDAR_USEPOLLING=1`, `nodemon --legacy-watch`, `--watch.usePolling`.
- **zsh eats a trailing `:ro`.** `jm set --mount $P:ro` and
  `-v $P:$P:ro` fail before the command runs, because zsh reads `:ro` as a
  history modifier. Quote the whole value: `"${P}:ro"`, `"${P}:${P}:ro"`.

## Permission denied writing to a share, or `git clone` fails in one

```text
fatal: Unable to create temporary file '/Users/me/src/x/.git/objects/pack/tmp_pack_XXXXXX': Permission denied
```

The host end of a 9p share runs as your ordinary Mac user, with no privilege
of its own. Under the `none` security model it applies the guest's modes
directly to the host file, so a process that creates a file read-only and then
writes to it — which is exactly what git does with its pack temp files — is
refused: macOS enforces the mode even for the file's owner, and being root in
the container buys nothing.

jm therefore starts shares with **`mapped-xattr`**, which keeps the guest's
ownership and modes in host xattrs so root in a container behaves as it does
on Linux. If you see this error, the machine was started with the override:

| Setting | Behaviour |
|---|---|
| `JM_9P_SECURITY` unset, or `mapped-xattr` | **Default.** Root in a container works; a container-created file looks `0600` on the Mac, with its real mode in `user.virtfs.mode` |
| `JM_9P_SECURITY=none` | Host-native modes on the Mac; a container that chmods its own files, `git clone` included, fails |

The value is read at `jm start` and is not stored in the machine record, so
one restart is the whole fix:

```bash
env | grep JM_9P_SECURITY     # is it set in your shell or profile?
jm stop && jm start           # restart without it
```

See [Modes and ownership on a share](USAGE.md#modes-and-ownership-on-a-share).

## Names do not resolve in the guest or in a container

The Mac's own resolver answers for the guest, so anything that resolves in
your browser should resolve in a container — VPN and split-horizon records,
`/etc/hosts` entries, `.local` names, your search domains.

```bash
jm doctor                                    # asserts parity against a host-only name
jm inspect | grep -i resolver                # is it running, and on what address
tail -50 ~/.jailmachine/machines/*/resolver.log
jm ssh -- host example.internal              # does the guest resolve it
jpodman run --rm --os=linux docker.io/alpine nslookup example.internal
```

| Symptom | Cause | Fix |
|---|---|---|
| `Resolver: stopped` in `jm inspect` | The host resolver did not come up; `jm start` warns and leaves the guest's previous resolution alone | `jm stop && jm start`, then read `resolver.log` |
| Resolves in the guest but not in a container | The engine gives containers their own resolver settings | `jm stop && jm start` re-pushes them; check `jm ssh -- cat /etc/resolv.conf` |
| A VPN name started failing after connecting | The search list is re-read every 30 s and pushed without a restart, but **containers already running keep the list they were created with** | Re-create the container |
| A short name fails, the fully-qualified one works | The search domain is not in the host's effective list | `scutil --dns` on the Mac is the source of truth |
| Nothing resolves at all in a container | The guest's `local_unbound` is down | `jm ssh -- service local_unbound status` |

jm never falls back to a public resolver when the host resolver errors: on a
split-horizon network that would answer an internal name with a public
address, and a wrong answer is worse than no answer. A failure is propagated
verbatim, so the guest fails exactly where the host would.

> A Linux container that runs its **own** resolver is fine as well: UDP
> sockets bind, send and receive normally under the Linuxulator, so a
> container doing its own lookups gets the same answers `local_unbound`
> would give it. See [UDP in a Linux container](#udp-in-a-linux-container).

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
| `resolver.log` | the host DNS resolver | A name resolves on the Mac but not in the guest or a container |
| `resolver.addr` | the same resolver | You want the `127.0.0.1:<port>` the guest forwards its queries to |
| `guest/shares.tab` | jm, at every start | You want the share table exactly as the guest reads it |

In the guest:

| Path | Contents |
|---|---|
| `/var/log/jm-provision.log` | Everything `provision.sh` did, both paths |
| `/var/db/jm-provisioned` | Ready marker — provisioning finished |
| `/var/db/jm-provision-failed` | Failure marker — the script aborted |
| `/var/log/podman_service.log` | The `podman system service` rc script |
| `/var/run/podman/podman.sock` | The engine API the host connects to |
| `/var/db/jm/conf/shares.tab` | The share table, mounted read-only from the host |
| `/var/log/jm-rtcsync.log` | The clock resync daemon that steps the guest from the EFI RTC |

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
