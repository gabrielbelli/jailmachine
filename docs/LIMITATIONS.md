# Limitations

Everything jailmachine cannot do, what it looks like when you hit it, whose
limitation it is, and what would have to change. Nothing here is a surprise
we are hiding: it is the map of the terrain, written so you can plan around
it before you commit a project to this stack.

The rule for this page is that **every claim is something somebody measured,
on a stated machine, on a stated day**. Where a limitation is inferred rather
than measured, it says so. Where a measurement contradicts something else in
this repository, the contradiction is
[listed at the end](#corrections-this-page-drove) rather than quietly
reconciled.

For how these limitations stack up against Docker Desktop and podman machine
on the same Mac, see [docs/COMPARISON.md](COMPARISON.md).

## Attribution key

Blaming the right layer matters more than the tone. Five owners appear in
every table:

| Whose | Means |
|---|---|
| **ours** | jailmachine's own design or code. We can fix it. |
| **FreeBSD** | The FreeBSD kernel or base system, usually the Linuxulator. A bare-metal FreeBSD container host has the same problem. |
| **podman-on-FreeBSD** | The podman/buildah/`ocijail` port, not the FreeBSD kernel and not us. Also reproduces on bare metal. |
| **upstream tooling** | gvproxy (gvisor-tap-vsock), QEMU, or the application's own code. |
| **Apple/macOS** | A host-platform constraint we cannot route around. |

Several rows carry two owners. That is honest, not hedging: the amd64 row is
a FreeBSD gap *and* an Apple restriction, and either one being lifted would
be enough.

## Where the numbers come from

| Session | Target | Host | Date |
|---|---|---|---|
| Compatibility survey, 46 image runs | the author's own machine, guest FreeBSD 15.1-RELEASE-p2 arm64, guest podman 5.8.4 | macOS 26.5.2 Apple Silicon, host podman 6.1.0, `jm` built from `f2a07f0` | 2026-08-21 |
| Limitation audit, syscall probes under `truss` | same machine, plus a throwaway `d-audit` (2 vCPU / 2048 MiB) | same | 2026-08-21 |
| Head-to-head against podman machine 6.1.0 and Docker Desktop 29.6.2 | `d-bench` (4 vCPU / 4096 MiB / 64 GiB) | Mac14,5 (M2 Max, 12 cores, 32 GiB), macOS 26.5.2 (25F84) | 2026-08-21, 18:13–21:10 UTC |
| Spot re-checks for this page | the author's machine, unchanged | same | 2026-08-21 |

Two caveats that apply to every number below. The benchmark Mac was **loaded
throughout** — Docker Desktop with 15 containers, two or three VMs — so
timings are pessimistic rather than clean-room. And the three stacks in the
head-to-head were each left at **their own defaults**, so their vCPU, RAM and
disk allocations differ; that comparison is "out of the box against out of
the box", not a controlled experiment.

---

## Images and architectures

> **The short version.** Native FreeBSD and Linux **arm64** images both run.
> `linux/amd64` images pull and then refuse to execute, and there is no
> emulation path on this platform — not because we declined to write one, but
> because neither of the two things that could provide it exists here.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| `linux/amd64` images | `pull --arch=amd64` succeeds, layers and all; `run` fails with `ocijail: error executing container command: Exec format error` | **FreeBSD** and **Apple** — see below | None found. Pull or build the `arm64` variant | Should be tracked; no issue yet |
| Linux images need `--os=linux` | Without it podman asks the registry for a FreeBSD variant and fails. The flag costs a registry round trip on every `run`: **1.87 s** against **0.39 s** for the same image under a `localhost/` tag | **podman** (client platform defaulting), triggered by the engine's OS being FreeBSD | `jdocker` sets `DOCKER_DEFAULT_PLATFORM=linux/arm64` and needs no flag. Under `jpodman`, tag locally or pre-pull | — |
| Docker Hub anonymous rate limits | `toomanyrequests: You have reached your unauthenticated pull rate limit`. Observed mid-survey; the same command took **33.8 s** instead of 1.87 s while throttled | **upstream tooling** (Docker Hub policy) — not a jm networking fault; `ghcr.io` was unaffected throughout | `podman login`, or pull from `mirror.gcr.io` / `public.ecr.aws` | — |
| FreeBSD image tags are arm64 here | A `:latest` FreeBSD image is the arm64 build; on an amd64 FreeBSD host you would want `:freebsd-amd64` | — (expected) | Name the architecture-specific tag | — |
| Intel Macs, Linux and Windows hosts | No backend. The Linux release binaries are build-only | **ours** (scope) — Apple Virtualization.framework cannot boot FreeBSD/arm64, which is why the backend is QEMU | None | — |
| GPU access | No `/dev/dri`, no `nvidia*`; a container's `/dev` has 11 nodes and none of them are a GPU. `--device /dev/dri` → `Error: stat /dev/dri: no such file or directory`. `--gpus all` is accepted and does nothing | **Apple** (no GPU passthrough into a QEMU/HVF guest) and **FreeBSD** | None | Should be tracked |

### Why `linux/amd64` has no emulation path

On Docker Desktop an amd64 image runs on Apple Silicon because the Linux
guest registers a translator with `binfmt_misc` — either QEMU's `qemu-user`
or Apple's Rosetta for Linux. Both routes are closed here, independently:

- **FreeBSD's Linuxulator has no `binfmt_misc`.** The equivalent mechanism,
  `imgact_binmisc`, exists in the kernel and is driven by `binmiscctl`, but
  `binmiscctl` is not installed in the guest and **we did not test** whether
  `emulators/qemu-user-static` under `imgact_binmisc` can execute a Linux
  amd64 binary through the Linuxulator. That is the one plausible route and
  it is genuinely unknown, not known-bad.
- **Rosetta is Linux-only and Virtualization.framework-only.** This Mac does
  ship `/Library/Apple/usr/libexec/oah/RosettaLinux`, but Apple delivers it
  to a *Linux* guest, over a *virtiofs* directory share, activated through
  `binfmt_misc`. jailmachine is QEMU with a FreeBSD guest and has none of
  those three things.

So "ours" applies only in the narrow sense that we choose not to ship an
emulation layer. There is currently no supported layer to ship.

---

## The Linuxulator

> **The short version.** Most of Docker Hub works: **41 of 46 image runs
> succeed — 33 unmodified, 8 after one flag or config line, 5 with no
> workaround found**. The failures are not random; five root causes explain
> every one of them, and each was isolated with a direct syscall probe from
> inside a container rather than guessed from an application's error text.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| `mremap` cannot grow a mapping | `linux_mremap(...) ERR#-12 'Cannot allocate memory'` under `truss`, for every flag combination including `MREMAP_MAYMOVE` | **FreeBSD** | Depends on the consumer — see the two rows below | Should be tracked |
| `node:22-alpine` hangs | `node --version` prints `v22.23.2` and exits 0. **Every other invocation** — `node -e ''`, `node -p 1+1`, `node -e 'fs.writeSync(1,…)'`, with or without a TTY — never returns, and is killed at the timeout. One thread spins at 100% of a core while four sleep on `futex` | **FreeBSD** — musl's `mallocng` grows its heap with `mremap` and retries forever | Use `node:22-bookworm-slim` (glibc), verified working including exit codes | Should be tracked |
| `apt-get` fails in Debian/Ubuntu containers | `E: Dynamic MMap ran out of room. Please increase the size of APT::Cache-Start. Current value: 25165824`, then `Problem with MergeList …Packages.lz4` | **FreeBSD** — APT's cache is the same `mremap` growth | **Yes**, one option — see below | Should be tracked |
| `signalfd4` returns `ENOSYS` | PostgreSQL ≥ 14: `FATAL: signalfd() failed` during initdb bootstrap | **FreeBSD** — the symbol is in `linux64.ko` but returns `ENOSYS` for every flag combination, and no `compat.linux.*` knob changes it | None. PostgreSQL has used `signalfd` unconditionally on Linux since v14 | Should be tracked |
| SysV IPC returns `ENOSYS` | PostgreSQL ≤ 13: `FATAL: could not create shared memory segment: Function not implemented`, `shmget(key=3143, size=56, 03600)` | **podman-on-FreeBSD** — `jls` shows the container's jail running `sysvshm=disable sysvsem=disable sysvmsg=disable`; the host kernel has `kern.features.sysv_shm: 1` and accepts a hand-made `jail -c … sysvshm=new` | None through podman; no flag exposes the jail parameter | Should be tracked |
| Nested `#!` interpreters | `mysql:9`: `error: exec failed: exec format error`. Its entrypoint is `#!/usr/bin/env bash` and that image's `/usr/bin/env` is itself `#!/usr/bin/coreutils …` — a two-level chain | **FreeBSD** — `imgact_shell` resolves one level, Linux's `binfmt_script` allows four. Control: the identical command in the identical image succeeds on Docker Desktop | `--user mysql`, so the entrypoint never re-execs through `gosu`; or `--entrypoint bash <script>` | Should be tracked |
| No `/proc/self/cgroup` | `mariadb:11`: `docker-entrypoint.sh: line 217: /proc/self/cgroup: No such file or directory`, then `set -e` aborts | **FreeBSD** (linprocfs) | Bypass the entrypoint, with `--user mysql` on **both** steps and a volume that persists between them — see below. The server itself is fine | Should be tracked |
| No Linux capabilities | `valkey:8-alpine`: `setpriv: activate capabilities: Operation not permitted` | **FreeBSD** | `--user valkey` (the entrypoint then skips `setpriv`), plus `--ignore-warnings ARM64-COW-BUG` | Should be tracked |
| `EPOLLEXCLUSIVE` unimplemented | nginx: `epoll_ctl(1, 6) failed (22: Invalid argument)`, `worker process exited with fatal code 2`, connection refused | **FreeBSD** — `linux_epoll` returns `EINVAL`, which nginx treats as fatal | `worker_processes 1;` **or** `accept_mutex on;` in the `events` block. Both re-verified to HTTP 200 | — |
| `io_uring_setup`, `pidfd_open` return `ENOSYS` | Application-dependent; usually a graceful fallback | **FreeBSD** | Most runtimes fall back on their own | — |
| busybox `nc -u -l` | `Address family not supported by protocol` | **FreeBSD** — a zero-length `recvmsg(MSG_PEEK)` returns at once on BSD (macOS behaves the same) where Linux blocks, so busybox connects to an all-zero sockaddr | `apk add netcat-openbsd`, or `socat`. UDP itself is sound | — |
| Redis' ARM64 COW probe | `Failed to test the kernel for a bug that could lead to data corruption during background save … Redis will now exit.` | **upstream tooling** — Redis' own probe, with its own documented switch | `redis-server --ignore-warnings ARM64-COW-BUG` → `+PONG` over a published port | — |
| Kernel modules from a container | `modprobe dummy` → `can't change directory to '/lib/modules'`; `lsmod` is empty | **FreeBSD** — a jail cannot load modules | `jm ssh -- kldload <module>` in the guest | — |

### The one syscall that explains two unrelated bugs

`linux_mremap` cannot grow a mapping. That single gap produces both the
`node:22-alpine` hang and the APT failure, and the APT half has a one-line
fix that was not written down anywhere before this page:

```bash
jpodman run --rm --os=linux docker.io/library/debian:trixie-slim \
  sh -c 'apt-get -o APT::Cache-Start=251658240 update -qq &&
         apt-get -o APT::Cache-Start=251658240 install -y -qq curl && curl --version'
# -> curl 8.14.1 (aarch64-unknown-linux-gnu) ...
```

Put the same option in a `Containerfile` and Debian-based builds work:

```dockerfile
RUN apt-get -o APT::Cache-Start=251658240 update \
 && apt-get -o APT::Cache-Start=251658240 install -y --no-install-recommends curl \
 && rm -rf /var/lib/apt/lists/*
```

### MariaDB, in full

Bypassing `mariadb:11`'s entrypoint takes two runs, and **both** need
`--user mysql`: run as root, `mariadb-install-db` reports `OK` and leaves a
root-owned datadir that `mariadbd` as `mysql` then cannot read, dying with
`Can't open and lock privilege tables: Table 'mysql.db' doesn't exist`. The
datadir also has to survive between the two runs, so it goes in a volume:

```bash
jpodman volume create mariadata
jpodman run --rm --os=linux -v mariadata:/var/lib/mysql --user mysql \
  --entrypoint mariadb-install-db docker.io/library/mariadb:11
jpodman run -d --os=linux -v mariadata:/var/lib/mysql --user mysql \
  -p 3306:3306 --entrypoint mariadbd docker.io/library/mariadb:11
```

That reaches `ready for connections. Version: '11.8.8-MariaDB-ubu2404'` and
answers the wire protocol from the Mac.

### It is not a musl problem

The temptation is to write "musl images do not work". That over-claims and
sends people to the wrong workaround. Measured on the same kernel, same day:
`alpine`'s busybox (musl-linked, confirmed with `ldd`), `python:3-alpine`,
`apk add curl` and `pip install requests` all work; `curlimages/curl`
(`aarch64-unknown-linux-musl`) works. Only node reaches the allocator path
that needs `mremap` to grow — and `nginx:1.31` on Debian/glibc fails
*exactly* as `nginx:1.31-alpine` does, which proves the nginx cause is not
musl either.

Attribute the node failure to `linux_mremap`, not to musl.

### PostgreSQL is the one genuine casualty

Two independent blockers with no version between them that clears both:
v14 and later need `signalfd`, v13 and earlier need SysV shared memory.
**PostgreSQL 14, 15 and 16 were not run** — 17 and 13 were, and the claim
that 14–16 also fail is an inference from PostgreSQL's own history, not a
measurement.

The obvious escape hatch is a native FreeBSD PostgreSQL
(`pkg install postgresql16-server` inside a FreeBSD container), which uses
neither Linux syscall. **That was not built or tested.** It is the single
most valuable open follow-up on this page.

### What definitely works

Confirmed present and working under probe: `eventfd2`, `timerfd_create`,
`inotify_init1`, `epoll_create1`, `memfd_create`, `statx`. Toolchains were
exercised properly rather than by `--version`: `go run` compiled and ran a
program in-container, `rustc` compiled and executed a binary, and
`git clone --depth 1 https://github.com/git/git.git` succeeded from a
container — DNS, TLS and egress through gvproxy all in one test.

---

## Filesystem sharing

> **The short version.** Host directories are shared at their **own absolute
> path**, so `-v /Users/you/src:/app` works from anywhere. The transport is
> virtio-9p, because **FreeBSD has no virtiofs driver at all** — and 9p is
> roughly **seven times slower than virtiofs on bulk I/O and ten times slower
> on metadata**, delivers no file-change events, and silently ignores
> `utimes`.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| Throughput | 200 MiB write **63 MB/s**, read **85 MB/s**, against **466 MB/s** and 3–5 GB/s for virtiofs on the same host directory. 1000 × 4 KiB files in **4.26 s** on the loaded benchmark Mac against 0.36–0.58 s (the figure the other pages quote, 3.6 s against 0.76 s on the guest's own ZFS, is the same test on a quiet host). `git clone --depth 1` in **4.72 s** against 0.26–0.31 s | **FreeBSD** (no virtiofs guest driver) and **upstream tooling** (QEMU virtio-9p, FreeBSD `p9fs`) | Keep build output, image layers and databases in an engine-managed volume on the guest's ZFS (`-v myvol:/out`) | [#4](https://github.com/gabrielbelli/jailmachine/issues/4) |
| File watching | Not "events go missing" — **the watch cannot be created**: `inotifywait: Couldn't watch /w: Bad file descriptor`. Controls in the same image succeed on the container's own filesystem and on a podman volume | **FreeBSD** — the Linuxulator's inotify works everywhere except `p9fs` | Polling watchers: `CHOKIDAR_USEPOLLING=1`, `nodemon --legacy-watch`, `--watch.usePolling` for Vite and Vitest. Or keep the watched tree in a volume | [#4](https://github.com/gabrielbelli/jailmachine/issues/4) |
| `utimes` | A silent no-op. `touch -t 200001010000` on a shared file leaves the mtime unchanged on both sides, with no error | **upstream tooling / FreeBSD** — the 9p `mapped-xattr` model | None. `make` and other mtime-driven tools can misbehave on a shared tree | [#4](https://github.com/gabrielbelli/jailmachine/issues/4) |
| Host-side modes look wrong | A file a container creates shows on the Mac as `0600`, with its real mode and owner in `user.virtfs.*` xattrs | **ours** — a deliberate trade. Under `security_model=none` a root container could not rewrite a file it had just made read-only, and `git clone` into a share failed outright | `JM_9P_SECURITY=none` (or `mapped-file`) at `jm start` trades it back. See [Modes and ownership on a share](USAGE.md#modes-and-ownership-on-a-share) | — |
| `/tmp` cannot be shared at its own path | `-v /tmp/x:/app` fails with `Error: OCI runtime error: ocijail: mounting {…}: source path does not exist: /tmp/x (create the directory first)`, exit `126`. podman never invents an empty directory for a source it cannot find; the silently-empty mount happens only in the other case, where that absolute path exists inside the guest as well and the guest's own copy is bound | **ours**, forced by macOS — `/tmp` is a symlink to `/private/tmp`, and a share at the guest's `/tmp` would shadow it | Write `-v /private/tmp/x:/app`. `$TMPDIR` (`/var/folders/...`) is shared by default and needs nothing | — |
| Everything shared is world-writable to containers | By default that is your whole home tree, `~/.ssh` and `~/.aws` included | **ours** — the same posture as Docker Desktop's default, chosen deliberately | On a **stopped** machine: `jm set --no-mounts`, then `jm set --mount ~/code --mount "/srv/data:ro"` for the roots you do want. `--no-mounts` cannot be combined with `--mount` in one call | — |
| A mangled `:ro` is accepted in silence | In zsh, `--mount $P:ro` **and** `--mount "$P:ro"` both arrive as `/Users/you/codeo` (`:r` is a history modifier). That path falls inside an already-shared root, so `jm set` absorbs it, exits `0`, and the share stays read-write. The only tell is the absence of the `==> share:` lines a real `--mount` prints | **ours** — jm cannot see what the shell already ate, but it could warn when a `--mount` adds nothing | Write `${P}:ro` or `$P\:ro`; quotes do not help. Confirm the mode in `jm inspect` afterwards | Should be tracked |
| No virtiofs at all | `kldload virtiofs` → `No such file or directory`; nothing matching `virtiofs`/`virtio_fs` in `/boot/kernel`. This QEMU 11.1.0 build also has no `vhost-user-fs` device | **FreeBSD** (no guest driver) and **upstream tooling** (this QEMU build) | None — 9p is the only transport, which is why the throughput row exists | [#4](https://github.com/gabrielbelli/jailmachine/issues/4) |

### Why 9p and not virtiofs

FreeBSD has no in-tree virtiofs driver. There is a working out-of-tree
implementation — Emil Tsalapatis' `virtiofs-head` branch, discussed on
freebsd-hackers in July 2024 —
[Re: Is anyone working on VirtFS (FUSE over VirtIO)](https://lists.freebsd.org/archives/freebsd-hackers/2024-July/003403.html),
tree at
[etsal/freebsd-src @ virtiofs-head](https://github.com/etsal/freebsd-src/tree/virtiofs-head).
It had not landed in the guest we run: on FreeBSD 15.1-RELEASE-p2 the module
does not exist. Until it does, virtio-9p is not a choice we made over
virtiofs; it is the only transport with a driver on both ends.

One honest note on the read figure. Docker Desktop's 5.2 GB/s read is served
from the **guest page cache** — the file had just been written. The fair
reading is not "virtiofs is 60× faster at reading" but "9p gets no such
caching in the guest at all": jailmachine's 85 MB/s read is barely above its
own 63 MB/s write.

---

## Networking

> **The short version.** TCP behaves. UDP has a **hard datagram ceiling**
> because gvproxy drops rather than fragments, and no container or guest IP
> is routable from the Mac — publish ports instead. Resolving *external*
> names is the one place jailmachine is measurably *better* than the
> alternatives; resolving another **container** by its name does not work at
> all, which is the sharpest edge on this page for anyone porting a compose
> file.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| UDP datagram ceiling | Datagrams above the link MTU minus 28 bytes vanish silently. No error, no `EMSGSIZE` | **upstream tooling** — gvproxy does not fragment at all | `JM_MTU` at `jm start` (clamped 576–16384) moves the wall; use TCP where you can. `jm doctor` states the exact ceiling per machine | [#2](https://github.com/gabrielbelli/jailmachine/issues/2) |
| No routable guest or container IP | A container gets `10.88.0.36` on the guest's CNI bridge. From the guest, `fetch http://10.88.0.36/` works; from the Mac, `curl` times out and `ping` is 100% loss. The guest's own `192.168.127.2` is equally unreachable | **ours** (ADR 0004 chose gvproxy) and **upstream tooling** (gvproxy is a userspace NAT with no host-side interface, by design). Docker Desktop on macOS has the same limitation | Publish ports with `-p`; `jm ports` shows every mapping and its error | Should be tracked |
| **Containers cannot resolve each other by name** | `nc: bad address 'redis'` from a sibling lookup. `podman network inspect podman --format '{{.DNSEnabled}}'` prints `false`; `/usr/local/libexec/cni/` holds `bridge firewall host-local loopback portmap static tuning` and no `dnsname` | **podman on FreeBSD** — the guest runs podman's **CNI** backend because `netavark` is not packaged for FreeBSD, and the CNI `dnsname` plugin is not packaged either. `aardvark-dns` 2.0.0 *is* in the repo but does nothing without netavark. A bare-metal FreeBSD container host behaves the same | A **Pod** — containers share a network namespace, so `localhost` works; this is what `podman kube play` gives you. (Pod mates' *container* names also land in `/etc/hosts`, but `kube play` names them `<pod>-<container>`, so prefer `localhost`.) In compose, `network_mode: "service:<name>"`. Otherwise `--add-host name:IP` / `extra_hosts:` with the bridge address from `podman inspect <name> --format '{{.NetworkSettings.IPAddress}}'`. External names, `/etc/hosts` and `host.containers.internal` are unaffected. See [USAGE](USAGE.md#containers-cannot-resolve-each-other-by-name) and [TROUBLESHOOTING](TROUBLESHOOTING.md#containers-cannot-resolve-each-other-by-name) | [#5](https://github.com/gabrielbelli/jailmachine/issues/5) |
| A published port is unreachable from the guest's own loopback | From inside the guest, `fetch http://127.0.0.1:<published>/` times out — and so does the guest's own `192.168.127.2:<published>` — while the Mac reaches the same container through jm's forwarder and the container's bridge address works from the guest. The CNI portmap `rdr` is present in `pfctl -a cni-rdr/<id> -sn` and does not apply to guest-originated traffic. Bites anyone pushing to a registry container | **podman on FreeBSD** (the CNI portmap plugin) | From the guest or another container, use the container's bridge address, or go back out through `host.containers.internal:<published>` — both verified. See [TROUBLESHOOTING](TROUBLESHOOTING.md#a-published-port-is-not-reachable-from-the-guest) | Should be tracked |
| No vsock | `kldload vmm_vsock` → `No such file or directory`; no `/dev/vsock`, no AF_VSOCK headers, no vsock device in this QEMU build | **FreeBSD** (no guest driver) | Already worked around: the engine socket is tunnelled over SSH (ADR 0001) | — |
| Published port is not instant | `run -p` to the first successful `curl` takes **2.29 s**, against 0.20 s and 0.24 s for podman machine and Docker Desktop. The forwarder reconciles a second or two after the container starts | **ours** — publishing is reconciled from guest state, not requested by the engine | `curl --retry 10 --retry-connrefused`, which the docs use everywhere | — |
| IPv6 in containers | `AAAA` queries answer NODATA | **ours** — the guest network is IPv4-only | None; v4 works throughout | — |
| A `0.0.0.0` hosts-file sinkhole answers empty | Docker Desktop and podman machine hand the container `0.0.0.0`; jailmachine returns an empty NOERROR | **ours**, deliberate (ADR 0008) — the name still fails to resolve, so the block holds | None needed | — |
| Running containers keep their search list | A container created before you joined a VPN keeps the search domains it was created with; the guest itself converges within 30 s | **ours**, documented in ADR 0008 | Recreate the container | — |

### The UDP ceiling, precisely

gvproxy drops an oversized datagram instead of fragmenting it, so the link
MTU is a hard wall rather than a performance knob. Our default MTU choice
**raises** that wall rather than causing it:

| Link MTU | Largest UDP payload that arrives | How you get it |
|---|---|---|
| 1500 (Docker's link size) | 1472 bytes | `JM_MTU=1500 jm start` |
| **9000 (our default)** | **8972 bytes** | nothing to set |
| 16384 (the clamp maximum) | 16356 bytes | `JM_MTU=16384 jm start` |

```bash
jm doctor | grep 'datagram limit'
# [ ok ]  datagram limit jailmachine   published udp carries payloads up to
# 8972 bytes (gvproxy MTU 9000); larger datagrams are dropped, not
# fragmented. $JM_MTU changes the link size (576..16384; JM_MTU=1500 matches
# Docker)
```

That is a default machine, with `$JM_MTU` unset. A value above 16384 clamps
to 16384; one below 576, or one that is not a number, falls back to 9000.
Native FreeBSD containers hit the same wall as Linux ones, which
is how we know it is the link and not the Linuxulator. Linux would deliver
65507 bytes by fragmenting. There is no MTU at which this behaves like
Linux — the wall only moves.

### Name resolution: the one row that goes the other way

Measured across all three stacks on 2026-08-21, resolving from inside a
container:

| Name | Host answer | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|---|
| `/etc/hosts` entry | `10.34.67.6` | 10.34.67.6 | 10.34.67.6 | 10.34.67.6 |
| mDNS `.local` name the host answers `127.0.0.1` for | 127.0.0.1 | **192.168.127.254** — the host, reachable | 127.0.0.1 — the container itself | 127.0.0.1 — the container itself |
| `host.docker.internal` | does not resolve | 192.168.127.254 | 192.168.127.254 | 192.168.65.254 |

jailmachine is the only one of the three that rewrites a host loopback answer
into an address a container can actually reach.

**Untested:** this Mac's `/etc/resolver` directory was empty and no entry was
created, so the split-horizon / VPN resolver claim in the README —
architecturally sound, since queries go through macOS's own resolution API —
**has not been verified end to end here**.

---

## Container lifecycle

> **The short version.** Restart policies **work**. Healthchecks **never
> fire**, and `--memory` and `--cpus` are accepted and then silently
> discarded — which is the more dangerous of the two, because nothing tells
> you.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| Healthchecks never fire | `--health-cmd true --health-interval 2s`, then after 14 s: `{"Status":"starting","FailingStreak":0,"Log":null}` — zero log entries, the timer never ran. `podman healthcheck run <name>` by hand returns `healthy` immediately | **podman-on-FreeBSD** — healthchecks are scheduled with systemd transient timers, and there is no systemd. A bare-metal FreeBSD container host behaves identically | `jm ssh -- podman healthcheck run <name>`, or a cron entry in the guest | [#3](https://github.com/gabrielbelli/jailmachine/issues/3) |
| `--memory` is not enforced | `--memory=64m` (and with `--memory-swap=64m`) still allocated a 400 MiB `bytearray`, twice. `podman inspect` records `Memory=0` — the value never reaches the runtime | **podman-on-FreeBSD** (no cgroups, `ocijail` applies nothing) with **FreeBSD** contributing: `kern.racct.enable: 0` and the kernel built `RACCT_DEFAULT_TO_DISABLED` | None per container. `jm set --memory` caps the whole VM, which is the honest granularity we can offer | Should be tracked |
| `--cpus`, `--cpuset-cpus`, `--cpu-shares` are not enforced | Same CPU-bound loop: **2.95 s** unrestricted, **2.91 s** at `--cpus=0.1`, **2.89 s** at `--cpuset-cpus=0`. `nproc` in a `--cpus=0.25` container still reports 2. `NanoCpus=0 CpuQuota=0` | **podman-on-FreeBSD**, as above | `jm set --cpus` for the whole VM | Should be tracked |
| `RUN --mount=type=cache` | `Error: … resolving mountpoints for container …: cache mounts not supported on freebsd` | **podman-on-FreeBSD** (buildah) | Restructure the Containerfile. `RUN --mount=type=secret` **does** work, verified | Should be tracked |
| Swarm | `docker swarm init` → `Error response from daemon: Podman does not support service: /v1.44/swarm/init` | **podman**, on every platform — not FreeBSD-specific | `jpodman kube play`, or compose | — |
| systemd as PID 1 | **Untested.** `--systemd=always` is accepted, and a container started with it has no `/sys/fs/cgroup`, no cgroup mounts and no `/run/systemd`, so a systemd PID 1 would have nothing to work with. The test image could not be built — `apt-get install systemd` hit the APT/`mremap` failure first | Expected **FreeBSD** / **podman-on-FreeBSD**, **not confirmed** | Retry with `APT::Cache-Start` set, or a pre-built systemd image | Should be tracked, once measured |
| Compose cannot ask for a platform per service | A Linux image under a plain `podman` compose run pulls the FreeBSD variant and fails | **upstream tooling** (compose has no per-service platform for this) | `jdocker compose` covers it via the wrapper's default platform; under `jpodman`, pre-pull with `--os=linux` plus `pull_policy: missing` | — |

### Restart policies do work — the docs are wrong

This is the one place where a measurement contradicts a shipped claim, so it
was run twice, by two people, on two days:

| Test | Result |
|---|---|
| `--restart=always`, container exits 3 every 2 s (audit, 2026-08-21) | `RestartCount` climbed 2 → 4 → 6 → 9 → 10 over 25 s |
| `--restart=on-failure:3` (audit) | Stopped at exactly 3 |
| Across `jm stop` + `jm start` (audit) | `--restart=always` came back `Up`; control `--restart=no` stayed `Exited (143)` |
| `--restart=always` re-run for this page (2026-08-21, author's machine) | `RestartCount=9 Status=running` after 22 s |

The README, `docs/USAGE.md` and issue
[#3](https://github.com/gabrielbelli/jailmachine/issues/3) all currently say
restart policies "apply only at boot". **They do not.** The healthcheck half
of #3 is correct and was reconfirmed for this page. #3 needs splitting or
correcting.

### Silent acceptance is the real problem

`--memory` and `--cpus` do not fail, warn, or appear in `podman inspect` as
anything but zero. A hardened compose file ports across unchanged and runs
with no limits at all. Treat the VM's own `--cpus` / `--memory` as your only
resource boundary and size the machine accordingly.

---

## Security and isolation

> **The short version.** A jail is the security boundary, and it is a real
> one — but **every Linux-shaped hardening flag you pass is accepted and
> silently ignored**. Nothing errors. Do not carry a threat model over from
> Docker on Linux without re-reading it here.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| `--privileged` does nothing | A privileged container gets exactly what an unprivileged one gets: byte-identical `/dev` (11 nodes), `mknod` → `Operation not permitted`, `mount -t proc` → `permission denied`. (`/proc/sys` *is* present — `fs`, `kernel`, `vm`, linprocfs-backed — and identical in both cases) | **podman-on-FreeBSD** / **FreeBSD** — a jail denies these regardless | None | Should be tracked |
| `--cap-add`, `--cap-drop` do nothing | `--cap-add=NET_ADMIN --cap-drop=ALL` runs without complaint or effect. `podman info` reports `capabilities:""` | **FreeBSD** — no Linux capabilities in the Linuxulator | None. Do **not** rely on `--cap-drop` for hardening here | Should be tracked |
| `--security-opt` does nothing | `seccomp=…`, `label=…`, `no-new-privileges` all accepted. `podman info` reports apparmor, seccomp and selinux all `false` | **FreeBSD** / **podman-on-FreeBSD** | None | Should be tracked |
| `--uidmap`, `--userns` do nothing | `--uidmap 0:100000:5000` runs as uid 0 and writes a file the guest sees as uid 0. `--userns=keep-id` → `failed to get current user: user: unknown userid -1` | **FreeBSD** (no user namespaces), surfaced by **podman-on-FreeBSD** | `--user 1000:1000` does work for dropping to a non-root uid inside the container | Should be tracked |
| Rootless is refused | `Error: rootless mode is not supported on FreeBSD - run podman as root` | **podman-on-FreeBSD** | None; it is at least an explicit refusal rather than a silent one | — |
| `:z` / `:Z` are no-ops | Accepted, no error, no relabelling — there is no SELinux to relabel | **podman-on-FreeBSD**, benign | None needed; compose files carrying `:z` port unchanged | — |
| Every container can read your home directory | The default share set includes `~`, and therefore `~/.ssh`, `~/.aws` and `~/.jailmachine` | **ours**, deliberate, the same default Docker Desktop ships | `jm init --no-mounts` at creation, or on a **stopped** machine `jm set --no-mounts` followed by `jm set --mount <root>`; `--mount /srv/data:ro` for read-only | — |
| Published ports reach your LAN by default | `-p 8080:80` binds every interface, as `docker run -p` does on Linux | **ours**, deliberate parity | `jm init --publish-addr 127.0.0.1`, or `jm set --publish-addr 127.0.0.1` | — |
| jm has root in the guest and writes firewall rules there | The forwarder loads `rdr` rules into the guest's `rdr/jm` pf anchor over the SSH control channel | **ours** — stated in ADR 0004's amendment | None; it is how a host-bound `-p` is made to work | — |

---

## Tooling and clients

> **The short version.** `jpodman` is the well-trodden path. The docker CLI
> works, but `jdocker build` fails out of the box on a Mac with Docker
> Desktop installed, for a reason that is one environment variable wide.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| `jdocker build` fails | `ERROR: failed to build: Error response from daemon: no image found in image index for architecture "arm64", variant "", OS "freebsd"`. The docker CLI routes `build` through buildx, which tries to boot `moby/buildkit` **inside the FreeBSD engine** | **upstream tooling** (no FreeBSD buildkit image) and **ours** — `jm` exports `DOCKER_DEFAULT_PLATFORM` for `jdocker` but not `DOCKER_BUILDKIT` | `DOCKER_BUILDKIT=0 jdocker build -t x .` succeeds; `jpodman build --os=linux -t x .` also succeeds | Should be tracked; one line in `internal/cli/docker.go` |
| `--os=linux` costs a registry round trip | Every `podman run` against a registry name: **1.87 s**, against 0.39 s for a `localhost/` tag and 0.38 s with no flag. Under a Docker Hub rate limit the same command took **33.8 s** | **podman on the host**, but jailmachine's documented workflow requires the flag | Tag locally, pre-pull, or use `jdocker` | Should be tracked |
| podman used to print an architecture error on every guest-side call | `level=error msg="Couldn't get cpu architecture: getCPUInfo for OS freebsd not implemented"` on stderr. **Not reproducible on guest podman 5.8.4** (the 15.1.0 prebaked image): `podman ps`, `info`, `version`, `images` and `system info` each produced 0 bytes of stderr, re-checked 2026-08-21. Recorded because an older guest may still show it | **podman-on-FreeBSD**, cosmetic | Ignore, or redirect stderr | — |
| No jail management from the host | There is no `jm jail`; jails are reached with `jm ssh -- bastille …` | **ours** (ADR 0006 scope) | `jm ssh -- bastille bootstrap 15.1-RELEASE`, etc. | — |
| No autostart at login | Nothing starts a machine at boot. `jpodman`/`jdocker` start a stopped machine on demand and nothing else does | **ours**, deliberate — `jm start` is one-shot and leaves four detached processes, which a launchd `KeepAlive` agent would fight | `JM_AUTOSTART=0` opts out of even the on-demand start | — |
| No snapshots, suspend, GUI or in-place guest upgrade | Not implemented | **ours** (ADR 0006 scope; ADR 0003 says re-init to move guest versions) | `jm rm && jm init` for a new guest image | — |

---

## The machine itself

> **The short version.** The VM is heavier and slower to start than the
> alternatives, and one of those numbers is a **bug of ours**: `jm init`
> writes about **47 GiB of real disk for an image with 3.17 GiB of content**,
> which is also why `init` is slow.

| Limitation | What you see | Whose | Workaround | Tracking |
|---|---|---|---|---|
| `jm init` allocates ~47 GiB | Settled free-space delta for one fresh booted machine: **46.87 GiB**; `st_blocks` agrees at 46.79 GiB; sampling 4000 random 4 KiB blocks of `disk.raw` finds **4.95 % non-zero (~3.17 GiB)**. One run in four landed at 12.87 GiB, the other three at 46–52 GiB | **ours** — the sparse writer in `internal/image/sparse.go` is not punching holes reliably | None today. Budget the disk, and `jm rm` machines you are not using | Should be tracked — the highest-value open bug on this page |
| `jm init` is disk-bound, not download-bound | From a locally cached `.zst` with no network at all: **59.1 / 63.3 / 113.0 s**. The 802 MiB download by itself is **31 s** by `curl` on this link | **ours**, same root cause as the row above | None | Should be tracked |
| Start and stop are slow | Warm start **36.7 s** on a loaded Mac against **10.4 s** for podman machine; stop **11.5 s** against 1.06 s. Almost all of it is guest boot | **ours** and **upstream tooling** (QEMU + FreeBSD boot, no fast-resume path) | None. The README's 12–25 s is a quiet Mac; 36.7 s is a busy one | — |
| The VM's RAM shows up in `ps` | Total host RSS at idle **196 MiB** (142 MiB of it QEMU); the QEMU process' `phys_footprint` ranges 1191–2902 MB for a 4096 MiB guest | **upstream tooling** — QEMU maps guest RAM as ordinary anonymous memory. Apple's Virtualization.framework does not, which is why `vfkit` reports 14 MB for a 2048 MiB guest. That 14 MB is not a real number and neither stack should be read as "uses less RAM" | None; it is accounting, not consumption | — |
| Disk grows only | `jm set --disk` extends; nothing shrinks | **ours** | `jm rm && jm init --disk` | — |
| One SSH port per machine | Running several machines means `jm init --ssh-port 2223 dev` and `JM_MACHINE=dev jpodman ps` | **ours** | As above | — |

Where jailmachine is competitive, for balance, measured in the same sitting:
image pull **6.35 s** against 3.55 s and 2.09 s; a five-step Debian build
**8.45 s** against 5.49 s and 6.56 s. It is not slow at the work; it is slow
at starting and at sharing files.

---

## Corrections this page drove

Six claims elsewhere in the repository were wrong when this page was written.
All six have been corrected in the docs; the issue tracker is the one place
that still lags.

| Was said | Measured | State |
|---|---|---|
| "restart policies apply only at boot" (README, `docs/USAGE.md`, [#3](https://github.com/gabrielbelli/jailmachine/issues/3)) | **False.** `RestartCount` climbs while the machine runs, `on-failure:N` honours N, and `always` survives `jm stop` + `jm start`. Confirmed twice | Docs corrected. **[#3](https://github.com/gabrielbelli/jailmachine/issues/3) still needs amending** — keep its healthcheck half, retract the restart half |
| "no `inotify` events reach a container" (README, `docs/USAGE.md`, [#4](https://github.com/gabrielbelli/jailmachine/issues/4)) | **Sharper and worse.** `inotify_add_watch` on a 9p path fails outright with `Bad file descriptor`; the watch is never created. inotify works fine on a volume and on the container's own filesystem | Docs corrected. **[#4](https://github.com/gabrielbelli/jailmachine/issues/4) still needs rewording** |
| node: `console.log` output "never reaches the pipe", HTTP servers "do not accept connections" (README, `docs/TROUBLESHOOTING.md`) | **`node --version` is the only thing that works.** `node -e ''` and `node -p 1+1` hang before reaching your script. Nothing partially works | Docs corrected |
| "`jm init` takes about 45–60 s, dominated by the roughly 800 MiB image download" (README) | **Neither half holds on this Mac.** 59–113 s from a *cached* image with no network; the download alone is 31 s. It is disk-write-bound | Docs corrected |
| node's failure framed as a musl problem (README, `docs/USAGE.md`) | Alpine busybox, `python:3-alpine`, `apk` and `pip` all work; Debian/glibc nginx fails identically to alpine nginx. The cause is `linux_mremap`, not musl | Docs corrected |
| "There is no host bind-mount: FreeBSD has no virtiofs driver, so `-v /Users/you/src:/app` cannot work" (`demo/volume/volume.sh`, `demo/README.md`) | **Stale.** Host bind-mounts work over 9p at identical paths and were used repeatedly across the compatibility survey | Docs corrected |

---

## What would have to change

Read as a roadmap, not an apology. Each area lists the concrete work — ours
or upstream — that would remove the limitation, in rough order of value.

### Images and architectures

| To remove | Work needed | Where |
|---|---|---|
| `linux/amd64` refusing to run | Test whether `emulators/qemu-user-static` registered through `imgact_binmisc`/`binmiscctl` can execute a Linux amd64 binary under the Linuxulator. If it can, install and register it in the guest image and the limitation is ours to close | **Us first** (one experiment), then FreeBSD if the answer is no |
| Rosetta as an alternative | Apple would have to deliver Rosetta for Linux outside Virtualization.framework, to a non-Linux guest, without a virtiofs share. Not plausible | Apple |
| `--os=linux` on every command | podman would need to infer the platform from the image rather than the engine's OS, or `jm` could register a connection carrying a default platform | podman upstream; a `jpodman` default is possible for us |

### The Linuxulator

| To remove | Work needed | Where |
|---|---|---|
| `node:22-alpine` hanging **and** `apt-get` failing | **One fix: make `linux_mremap` grow a mapping.** Both bugs disappear together. This is the single highest-value upstream change on the page | FreeBSD `sys/compat/linux` |
| PostgreSQL ≥ 14 | Implement `signalfd4` rather than returning `ENOSYS`. Reading `sys/compat/linux` for 15.1 would first settle whether it is a stub or gated | FreeBSD |
| PostgreSQL ≤ 13 | `ocijail` would need to stop hard-disabling `sysvshm`/`sysvsem`/`sysvmsg`, or expose a flag. The kernel already accepts `jail -c … sysvshm=new` | podman-on-FreeBSD (`ocijail`) |
| PostgreSQL, today, without either | Build and document a **native FreeBSD PostgreSQL container** (`pkg install postgresql16-server`). Neither syscall is involved. Untested — the most valuable follow-up we can do ourselves | **Us** |
| `mysql:9`'s entrypoint | Let `imgact_shell` resolve up to four `#!` levels, as Linux's `binfmt_script` does | FreeBSD |
| `mariadb:11`'s entrypoint | Add a `cgroup` file to linprocfs, even a static one | FreeBSD |
| `valkey`'s `setpriv` | Linux capabilities in the Linuxulator — a large piece of work, unlikely | FreeBSD |
| nginx out of the box | `EPOLLEXCLUSIVE` in `linux_epoll`, or nginx treating `EINVAL` there as non-fatal | FreeBSD, or nginx upstream |
| The rest | Keep the compatibility matrix measured and the workarounds documented; that is the honest deliverable while the syscall surface fills in | **Us** |

### Filesystem sharing — [#4](https://github.com/gabrielbelli/jailmachine/issues/4)

| To remove | Work needed | Where |
|---|---|---|
| 9p throughput, and the metadata cost that dominates builds | Land a virtiofs guest driver in FreeBSD — the [out-of-tree branch](https://github.com/etsal/freebsd-src/tree/virtiofs-head) exists and was discussed on [freebsd-hackers](https://lists.freebsd.org/archives/freebsd-hackers/2024-July/003403.html) — **and** build QEMU with `vhost-user-fs`, which this Homebrew build lacks | FreeBSD and upstream QEMU packaging |
| The same, in jailmachine | ADR 0007 already makes sharing a backend capability with share descriptors, so switching transport is a backend change, not a CLI change. When the driver lands we swap it and the identity-path rule is untouched | **Us**, cheaply, once the driver exists |
| File watching | virtiofs would not fix it by itself — change notification has to come from somewhere. Realistically: polling until FUSE-side notifications exist | FreeBSD |
| `utimes` being a no-op | 9p `mapped-xattr` semantics; goes away with virtiofs | FreeBSD |
| Host-side `0600` files | Nothing, unless the share runs as a privileged host process. `JM_9P_SECURITY=none` already offers the other trade | — |

### Networking — [#2](https://github.com/gabrielbelli/jailmachine/issues/2)

| To remove | Work needed | Where |
|---|---|---|
| The UDP datagram ceiling | gvproxy would have to fragment, or **jm would have to stop needing gvproxy**: a vmnet-based NetworkProvider on macOS would give real link semantics | gvproxy upstream, or **us** behind the existing NetworkProvider interface (ADR 0004) |
| Unroutable container and guest IPs | The same vmnet/bridged provider. ADR 0004 already anticipates it: "a provider that yields a LAN-routable address can make the forwarder a no-op; the CLI surface does not change" | **Us** |
| The 2.29 s publish delay | Shorten the reconcile debounce, or have the forwarder act on the `podman start` event before the container is fully up | **Us**, small |
| No vsock | A FreeBSD AF_VSOCK guest driver. Until then the SSH tunnel stays, and it works | FreeBSD |
| The split-horizon claim being untested | Create an `/etc/resolver` entry on a test Mac and verify a scoped resolver end to end. No code needed, just the measurement | **Us** |
| Containers not resolving each other — [#5](https://github.com/gabrielbelli/jailmachine/issues/5) | Upstream: package `netavark` for FreeBSD (then `aardvark-dns`, already packaged, becomes useful), or package the CNI `dnsname` plugin. Sooner and ours: jm already owns the guest's resolver and runs `local_unbound` there, so the forwarder — which already watches `podman ps`/`podman events` for publishing — could push `name -> IP` records into it as containers come and go. Container `/etc/resolv.conf` already points at a resolver we configure | podman-on-FreeBSD / FreeBSD ports, or **us** |
| A published port being unreachable from the guest's own loopback | The CNI portmap plugin installs the `rdr` but it does not fire for guest-originated traffic. Either fix it upstream, or teach the guest side to hairpin | podman-on-FreeBSD (`containernetworking-plugins`) |

### Container lifecycle — [#3](https://github.com/gabrielbelli/jailmachine/issues/3)

| To remove | Work needed | Where |
|---|---|---|
| Healthchecks never firing | podman on FreeBSD needs a timer source that is not systemd — either an in-process scheduler in `podman system service`, or a rc/cron-driven runner | podman-on-FreeBSD upstream |
| The same, sooner | A `jm`-owned healthcheck runner in the guest that walks containers with a healthcheck defined and calls `podman healthcheck run`. Small, self-contained, and removes the sharpest edge in #3 | **Us** |
| `--memory` / `--cpus` being ignored | `ocijail` would have to map them to `rctl`, and the guest would need `kern.racct.enable=1` (a tunable `jm` could set in the image). Both halves are needed — flipping the tunable alone changes nothing | podman-on-FreeBSD, then **us** for the tunable |
| Silent acceptance of those flags | Failing or warning on a limit that cannot be applied is a small patch and a large usability win | podman-on-FreeBSD upstream |
| `RUN --mount=type=cache` | buildah FreeBSD support for cache mounts | podman-on-FreeBSD upstream |
| Issue #3 being half wrong | Split it: keep the healthcheck half, retract the restart half | **Us**, today |

### Security and isolation

| To remove | Work needed | Where |
|---|---|---|
| Hardening flags being silently ignored | The right fix is **not** to implement Linux capabilities on FreeBSD. It is for podman-on-FreeBSD to **reject or warn** on `--privileged`, `--cap-*`, `--security-opt` and `--uidmap` instead of accepting them. Silent acceptance is what makes this dangerous | podman-on-FreeBSD upstream |
| The same, sooner | `jm doctor` could state the posture in one line, and the docs say it plainly — which this page now does | **Us** |
| Real per-container isolation beyond a jail | User namespaces and capabilities in FreeBSD. Long-horizon | FreeBSD |
| The default share posture | Nothing to fix — `jm init --no-mounts` and `jm set --no-mounts` both exist and are now documented on the pages a worried reader reaches first | **Us**, done |

### Tooling

| To remove | Work needed | Where |
|---|---|---|
| `jdocker build` failing | Export `DOCKER_BUILDKIT=0` alongside `DOCKER_DEFAULT_PLATFORM` in `internal/cli/docker.go`. One line. Severity is install-dependent — it depends on which buildx builder is active — but it reproduced on this Mac with the stock Docker Desktop arrangement | **Us** |
| BuildKit builds properly | A FreeBSD `moby/buildkit` image | upstream tooling |
| The `getCPUInfo` stderr noise | `getCPUInfo` for FreeBSD in podman | podman-on-FreeBSD upstream |
| Host-side jail management | A `jm jail` surface, which ADR 0006 deliberately deferred as a second product | **Us**, post-MVP |

### The machine itself

| To remove | Work needed | Where |
|---|---|---|
| ~47 GiB written for 3.17 GiB of content, and the slow `init` that follows from it | Fix hole-punching in `internal/image/sparse.go`, then re-measure the free-space delta and the `init` time. Both numbers should collapse together, and the README's timing claim becomes true again | **Us** — the highest-value bug on this page |
| Slow warm start | Profile the guest boot; there is no fast-resume path under QEMU today, and `jm start` deliberately holds no daemon | **Us**, then upstream QEMU/FreeBSD |
| No fast machine suspend | QEMU savevm against a running FreeBSD guest, plus state-model work in ADR 0005 | **Us**, post-MVP |
| Other host platforms | A Linux backend (QEMU + KVM, same argv) and a Windows one (Hyper-V) behind ADR 0002's backend interface | **Us**, post-MVP |

---

## Reporting something not on this page

Open an issue with the exact command, the full output, and `jm doctor`'s
report. If the failure is inside a Linux container, a `truss` of the process
in the guest is what turns "it hangs" into an attributable syscall — that is
how both the `mremap` and the `signalfd4` findings on this page were pinned
down, and it is the difference between a bug we can route to FreeBSD and one
that sits unexplained.
