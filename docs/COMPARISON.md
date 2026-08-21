# jailmachine vs Docker Desktop vs podman machine

macOS on Apple Silicon, measured on one Mac on **2026-08-21**. This page
exists so you can decide **against** jailmachine as easily as for it: most of
the rows below are losses, and every loss names whose it is.

## Summary

If you run Linux containers on a Mac and nothing else, **use Docker Desktop
or podman machine** — both are faster than jailmachine at every measured
engine operation, and their file sharing is about 7× faster on bulk writes
and 7–18× faster on metadata-heavy work. Choose **Docker Desktop** if you
want a GUI, a bundled Kubernetes, BuildKit and an ecosystem of extensions;
choose **podman machine** if you want the smallest, quietest, free CLI-only
Linux engine (two host processes against Docker Desktop's twelve, a 10.4 s
warm start, a 2.41 GiB machine). Choose **jailmachine** only when the guest
kernel has to be FreeBSD: native FreeBSD OCI images, `bastille` jails, or
testing how software behaves on FreeBSD — none of which the other two can do
at all. If that is not your problem, jailmachine is the wrong tool, and this
page will show you why in numbers.

## Choose this if

| You want | Tool | Why, in one line |
|---|---|---|
| Native FreeBSD OCI images on a Mac | **jailmachine** | The only one of the three with a FreeBSD kernel; `ghcr.io/freebsd/freebsd-runtime:15.1` runs with no flag |
| Jails (`bastille`) on a Mac | **jailmachine** | Shipped and configured in the guest, reached with `jm ssh` (not measured in this comparison — see [Jails](#jails-and-native-freebsd-images)) |
| To check how your image behaves under FreeBSD's Linuxulator | **jailmachine** | 41 of 46 image runs work; the 5 failures have named root causes |
| Fast Linux containers, GUI, Kubernetes, BuildKit, extensions | **Docker Desktop** | Fastest measured pull (2.09 s) and port round trip (0.24 s); by far the largest footprint (1695 MiB host RSS, 12 processes) |
| Fast Linux containers, small and quiet, no daemon on the host | **podman machine** | 10.4 s warm start, 1.06 s stop, 2.41 GiB machine, 2 host processes |
| `linux/amd64` images on Apple Silicon | **Docker Desktop** or **podman machine** | jailmachine cannot run them at all (**FreeBSD**: no `binfmt_misc`, no Rosetta path) |
| Enforced `--memory` / `--cpus` per container | **Docker Desktop** or **podman machine** | jailmachine accepts the flags and ignores them (**podman-on-FreeBSD**: no cgroups) |
| Healthchecks and container-level supervision | **Docker Desktop** or **podman machine** | Healthchecks never fire on FreeBSD ([#3](https://github.com/gabrielbelli/jailmachine/issues/3)) |
| Heavy file I/O between the Mac and the container | **Docker Desktop** or **podman machine** | virtiofs against jailmachine's 9p: 466 MB/s vs 63 MB/s writing ([#4](https://github.com/gabrielbelli/jailmachine/issues/4)) |

> Not measured, public knowledge: Docker Desktop requires a paid subscription
> for larger organisations; jailmachine is BSD-2-Clause and podman machine is
> Apache-2.0. Check Docker's current terms yourself — licensing was not part
> of this benchmark.

## Not measured here: Lima, Colima and OrbStack

Three obvious alternatives were **not installed, not run and not measured**
for this page; what follows is public knowledge, offered only so the field is
not misrepresented by omission. **Lima** boots a Linux guest on macOS through
Virtualization.framework or QEMU and is what most other tools are built on;
**Colima** is a container-focused wrapper around Lima giving a docker or
containerd runtime with a one-line start; both are open source, CLI-only, and
occupy roughly the same niche as podman machine — expect similar start-up and
virtiofs-class sharing, but do not quote that expectation as a measurement.
**OrbStack** is a commercial macOS-only product whose selling point is exactly
the axis jailmachine loses on — fast start, low idle footprint and fast file
sharing — and it is the tool to compare against if those are your criteria.
None of the three run a FreeBSD guest: Lima and Colima boot Linux, OrbStack
runs Docker containers and Linux machines, and Virtualization.framework
cannot boot FreeBSD/arm64 at all, which is why jailmachine uses QEMU
([tech-choices](tech-choices.md)).

## How these numbers were produced

| | |
|---|---|
| Host | Mac14,5 (Apple M2 Max, 12 cores, 32 GiB), macOS 26.5.2 (25F84) |
| Date | 2026-08-21, 18:13–21:10 UTC in one sitting |
| jailmachine | built from `f2a07f0`; machine `d-bench`, 4 vCPU / 4096 MiB / 64 GiB, QEMU + HVF, FreeBSD 15.1-RELEASE-p2 arm64, guest podman 5.8.4 |
| podman machine | 6.1.0, `podman-machine-default`, 6 vCPU / 2048 MiB / 100 GiB, vfkit / Virtualization.framework, Fedora CoreOS, rootless — created for this benchmark and removed afterwards |
| Docker Desktop | 29.6.2, compose v5.3.1, 10240 MiB, `com.docker.krun` |
| Repetitions | Median of three unless a row says otherwise |

Read every figure with these five caveats:

| Caveat | Consequence |
|---|---|
| The three VMs use **each tool's defaults**, not matched resources | podman machine has 2 more vCPUs (helps its build row); Docker Desktop has 2.5× the RAM (helps its read row) |
| **RSS means different things per hypervisor** | QEMU maps guest RAM as ordinary anonymous memory and it shows in `ps`; Virtualization.framework does not, which is why vfkit reports 14 MB for a 2048 MiB guest. **Do not read "podman machine uses 28 MiB" as a real number** |
| Docker Desktop ran **15 of the user's containers throughout** | Its footprint row includes them; its lifecycle rows were skipped rather than disturb them |
| The host was **loaded the whole time** (three VMs, 15 containers) | jailmachine's 36.7 s warm start should be read against the README's 12–25 s on a quiet Mac; a quiet Mac was not available |
| Docker Hub rate-limited this IP mid-session | Rows 3–7 and the DNS rows use `public.ecr.aws/docker/library/…` — the same bytes from an unthrottled mirror — for all three stacks |

---

## Start-up and footprint

| # | Measurement | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|---|
| 1 | Cold start, image already cached (`init` + `start`) | 98.3 s (init 63.3 + start 35.0) | 56.2 s (init 40.6 + start 15.6), n=1 | not measured |
| 1b | Cold start including the first-ever image download | 513.9 s, n=1 (init 479.5 + start 34.3) | included in the 56.2 s above | not measured |
| 1c | That image download on its own, by `curl` | 31.0 s / 802 MiB | ~40 s / 896 MiB, inside `init` | — |
| 2 | Warm start | 36.7 s | 10.4 s | not measured |
| 2b | Stop | 11.5 s | 1.06 s | not measured |

Docker Desktop's lifecycle rows are absent because restarting its VM would
have killed the user's running containers; `docker desktop stop`/`start`
exists and would have given a number, and it was not used. podman machine's
cold start is n=1 because its image cache persists across `init`, so runs two
and three would have measured something different. jailmachine's one true
end-to-end cold run (513.9 s) happened with the host volume at 99 % full and
other work running; treat it as a worst case, not a typical one.

### Idle footprint, after 60 s idle

| | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|
| Host processes | 6 (`qemu-system-aarch64`, `gvproxy`, forwarder, resolver, `ssh`, shell) | 2 (`vfkit`, `gvproxy`) | 12 (`com.docker.krun`, 3 × `com.docker.backend`, UI, agents, `vmnetd`) |
| Total host RSS | 196 MiB (142 MiB of it QEMU); 602 MiB right after boot | 28 MiB — see the RSS caveat | 1695 MiB |
| VM process `phys_footprint` | 1191–2902 MB (guest configured 4096 MiB) | 14 MB — guest RAM not attributed to the process | 18 GB (guest configured 10240 MiB, plus 15 containers) |
| Disk, settled free-space delta on removal | 46.87 GiB for one fresh machine | 2.41 GiB, plus 0.87 GiB of image cache kept after `machine rm` | 44 GiB (`Docker.raw`) |
| Non-zero content of that disk image | 3.17 GiB (4000-block sample) | — | — |

Docker Desktop's 44 GiB is a two-year-old working installation (25 GB of
images, 32 containers, 32 volumes, 3.7 GB of build cache), not a fresh-install
baseline, and is **not** comparable to the two day-zero machines beside it.

**`jm init` writes about 47 GiB of real disk for 3.17 GiB of content, and that
is jailmachine's own bug** (**ours**, `internal/image/sparse.go` is not
punching holes reliably: free-space delta 46.87 GiB, `st_blocks` 46.79 GiB,
4.95 % of 4000 sampled 4 KiB blocks non-zero; one run in four landed at 12.87
GiB instead). It also explains why `init` is slow: from a **locally cached**
`.zst` with no network at all it took 59.1 / 63.3 / 113.0 s, so it is
disk-write-bound, not download-bound. The README used to attribute the cost
to the download; it now quotes **60–115 s** and names the disk write, on the
strength of these numbers. Not yet tracked by an issue.

> **Interpretation:** jailmachine is three to four times slower to start and
> stop than podman machine and writes twenty times more disk than it needs
> to; the gap in `init` is a fixable bug of ours, the gap in boot time is
> QEMU + a full FreeBSD boot against a purpose-built CoreOS image and will
> not close by much.

---

## Image compatibility

Measured against the author's running machine, FreeBSD 15.1-RELEASE-p2 arm64,
guest podman 5.8.4, `compat.linux.osrelease=5.15.0`. **46 image runs, single
run each, no repetitions.**

| Verdict | Count |
|---|---|
| Works unmodified | 33 |
| Works with a flag or one config line | 8 |
| Broken, no workaround found | 5 |

Docker Desktop and podman machine run a Linux kernel, so all 46 are expected
to work there and only two were used as controls (the two-level `#!` chain,
and the literal build Containerfile). Every failure below is a **Linuxulator
or podman-on-FreeBSD** gap, and each was isolated with a direct syscall probe
rather than inferred from the application's error text.

| Root cause | Breaks | Whose |
|---|---|---|
| `signalfd4` → `ENOSYS` | every PostgreSQL ≥ 14 | **FreeBSD** (Linuxulator; symbol present in `linux64.ko`, no `compat.linux.*` knob) |
| SysV IPC (`shmget`/`semget`/`msgget`) → `ENOSYS` | PostgreSQL ≤ 13 | **podman-on-FreeBSD** — `ocijail` runs the jail with `sysvshm=disable sysvsem=disable sysvmsg=disable` and exposes no flag; the same kernel accepts a hand-made `jail -c … sysvshm=new` |
| `#!` chains deeper than one level → `ENOEXEC` | `mysql:9` entrypoint | **FreeBSD** — `imgact_shell` resolves one level, Linux's `binfmt_script` allows four. Control: the identical command in the identical image prints `two-level-ok` on Docker Desktop |
| No `/proc/self/cgroup` | `mariadb:11` entrypoint | **FreeBSD** (linprocfs) |
| No Linux capabilities | `valkey:8-alpine` entrypoint (`setpriv`) | **FreeBSD** (Linuxulator) |
| `linux_mremap` cannot grow a mapping (`ENOMEM` always, `MREMAP_MAYMOVE` included) | `node:22-alpine` hangs; `apt-get` fails in Debian/Ubuntu | **FreeBSD** |

Also confirmed `ENOSYS`: `io_uring_setup`, `pidfd_open`. Confirmed working:
`eventfd2`, `timerfd_create`, `inotify_init1`, `epoll_create1`, `memfd_create`,
`statx`.

What that means for real images:

| Image class | jailmachine | Docker Desktop / podman machine |
|---|---|---|
| Base OS (`alpine`, `debian`, `ubuntu`, `fedora`, `rockylinux`, `busybox`) | Works | Works |
| Toolchains (`golang:alpine`, `rust:1-slim`, `python`, `ruby`, `php`, `temurin:21-jre`) | Works — `go run`, `rustc` and `git clone` were exercised, not just `--version` | Works |
| `node:22-bookworm-slim` (glibc) | Works | Works |
| `node:22-alpine` (musl) | **Broken.** `node --version` prints and exits 0; every other invocation hangs until killed | Works |
| Web servers (`httpd`, `caddy`, `traefik`, `grafana`, `prometheus`) | Works, answered on a **published port from the Mac** | Works |
| `nginx` (alpine **and** Debian) | Works with one config line — `worker_processes 1;` or `accept_mutex on;` → HTTP 200. **FreeBSD**: `linux_epoll` has no `EPOLLEXCLUSIVE` | Works stock |
| `redis:8-alpine` | Works with `--ignore-warnings ARM64-COW-BUG` (**upstream tooling** — Redis' own probe, its own documented switch) | Works stock |
| `valkey:8-alpine` | Works with `--user valkey` plus the same switch | Works stock |
| `mysql:9`, `mariadb:11` | Servers run and answer their wire protocol from the Mac; only the **entrypoint scripts** fail. `--user mysql` fixes MySQL; MariaDB needs its entrypoint bypassed | Works stock |
| `mongo:8`, `rabbitmq:4`, `memcached` | Works stock, answered from the Mac | Works |
| PostgreSQL, any version | **Broken.** Two independent blockers with no version clearing both | Works |
| `apt-get update` in a Debian container | **Broken stock.** `E: Dynamic MMap ran out of room … Current value: 25165824`. Fixed by `-o APT::Cache-Start=200000000`, verified installing curl | Works |
| `linux/amd64` anything | **Broken.** `pull` succeeds, `run` → `ocijail: error executing container command: Exec format error` | Works (emulated) |
| Native FreeBSD images (`ghcr.io/freebsd/freebsd-runtime:15.1`, `:14.3`, `dougrabson/freebsd15-minimal`) | Works, no flag | **Cannot run at all** |

> **Interpretation:** the Linux side of jailmachine is a good deal better than
> "it sort of works" and a good deal worse than parity — servers and
> toolchains are fine, PostgreSQL and amd64 are not, and the things that break
> break in entrypoint scripts rather than in the engines themselves.

### The `--os=linux` tax

jailmachine's documented workflow requires `--os=linux` on the host podman for
every Linux image, and that flag costs a **registry round trip on every
`podman run`**, even for an image already in the guest:

| Same container, same machine | Time |
|---|---|
| `--os=linux`, registry-qualified name | 1.87 s |
| `--os=linux`, `localhost/` tag (no registry check) | 0.39 s, n=1 |
| no flag | 0.38 s |
| `--os=linux` while this IP was rate-limited by Docker Hub | 33.8 s |

Whose: **podman-on-the-host** behaviour, but it is jailmachine's workflow that
forces the flag, so the cost lands on jailmachine's users. `jdocker` avoids it
by setting `DOCKER_DEFAULT_PLATFORM` instead. Not yet tracked by an issue.

---

## Filesystem sharing

Same host directory under `/private/tmp`, same alpine image, all three stacks.

| Operation | jailmachine (9p) | podman machine (virtiofs) | Docker Desktop (virtiofs) |
|---|---|---|---|
| Write 200 MiB, `dd conv=fsync` | 3.33 s — **63 MB/s** | 0.45 s — 466 MB/s | 0.45 s — 466 MB/s |
| Read 200 MiB (guest cache warm) | 2.47 s — **85 MB/s** | 0.07 s — 2996 MB/s | 0.04 s — 5242 MB/s |
| Create 1000 × 4 KiB files | **4.26 s** | 0.58 s | 0.36 s |
| `git clone --depth 1` of this repo | **4.72 s** | 0.31 s | 0.26 s |

The read row is served from the guest's page cache on all three — the file had
just been written — which is why Docker Desktop shows 5.2 GB/s. It measures
caching, not the share. The honest reading is that jailmachine's 9p gets no
such caching in the guest at all: 85 MB/s reading is barely above its own
write speed. These figures reproduce the recorded jailmachine numbers (72
MB/s write, 89 MB/s read, 4 s clone) within noise; the small-file case came out
slower here (4.26 s against the 3.6 s on record) with three VMs and 15 containers
live, and it used a shell `printf` builtin rather than `dd`, so it is not
strictly the same measurement.

Beyond speed, three semantic gaps, all on the jailmachine side:

| Gap | Measured | Whose |
|---|---|---|
| `inotify` does not merely miss events — **the watch cannot be created**: `inotifywait: Couldn't watch /w: Bad file descriptor` | Controls in the same image succeed on the container's own filesystem and on a podman volume | **FreeBSD** — the Linuxulator's inotify does not work over `p9fs`. [#4](https://github.com/gabrielbelli/jailmachine/issues/4) should be reworded: it fails at `inotify_add_watch` |
| `utimes` is a silent no-op | `touch -t 200001010000` left the mtime unchanged on both sides, no error | **upstream tooling / FreeBSD** — 9p `mapped-xattr` |
| A container-created file appears on the Mac as `0600`, real mode in `user.virtfs.*` xattrs | By design (ADR 0007 amendment) — it is what makes `git clone` into a share work at all | **ours**, deliberately |

There is no virtiofs to switch to: `kldload virtiofs` → `No such file or
directory`, nothing matching `virtiofs` in `/boot/kernel`, and this QEMU
11.1.0 build has no `vhost-user-fs` device either (**FreeBSD** for the driver,
**upstream tooling** for the build).

> **Interpretation:** this is jailmachine's largest and least fixable loss. If
> your workflow is "edit on the Mac, build in the container, watch for
> changes", the other two are 5–13× faster and their file watchers work.

---

## Networking and DNS

All three route container traffic through a userspace NAT and none of them
gives the Mac a route to container IPs. Where they differ is what a name
resolves to.

| Name | Host answer | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|---|
| `auth.catallaxy.internal` (`/etc/hosts`) | 10.34.67.6 | 10.34.67.6 | 10.34.67.6 | 10.34.67.6 |
| `vk.gabrielbelli.com` (`/etc/hosts`) | 192.168.1.1 | 192.168.1.1 | 192.168.1.1 | 192.168.1.1 |
| `darwin.local` (mDNS) | 127.0.0.1 | **192.168.127.254** — the host, reachable | 127.0.0.1 — the container itself | 127.0.0.1 — the container itself |
| `ic.adobe.io` (hosts sinkhole → `0.0.0.0`) | 0.0.0.0 | empty NOERROR (deliberate, [ADR 0008](adr/0008-name-resolution-parity.md)) | 0.0.0.0 | 0.0.0.0 |
| `host.docker.internal` | does not resolve | 192.168.127.254 | 192.168.127.254 | 192.168.65.254 |
| `host.containers.internal` | does not resolve | 192.168.127.254 | 192.168.127.254 | does not resolve |
| `nonexistent.test` | NXDOMAIN | none | none | none |

All three honour the Mac's `/etc/hosts`. jailmachine is the only one that
rewrites a host loopback answer into an address the container can actually
reach; the other two hand back `127.0.0.1`, which inside a container means the
container. jailmachine drops a `0.0.0.0` sinkhole answer instead of echoing
it — the name still fails to resolve, so the block holds.

**This Mac had no `/etc/resolver` entries and none were created**, so the
split-horizon / VPN claim in the README is **untested here**. What could be
tested — hosts entries, an mDNS name, a sinkholed name — is above.

Two more measured facts, both jailmachine-side:

| | Measured | Whose |
|---|---|---|
| Container IPs are not routable from the Mac | Container at `10.88.0.36` answers from the guest, `curl` from the Mac times out, `ping` 100 % loss. The guest's own `192.168.127.2` is equally unreachable | **ours** (ADR 0004 chose gvproxy) and **upstream tooling** (gvproxy is a userspace NAT with no host-side interface). Docker Desktop on macOS has the same limitation |
| UDP datagrams above the link MTU are dropped silently | 8972 bytes at the default MTU 9000; 16356 on a machine running `JM_MTU=16384`. Same for native FreeBSD and Linux containers, so it is the link | **upstream tooling** — gvproxy does not fragment. [#2](https://github.com/gabrielbelli/jailmachine/issues/2) |

> **Interpretation:** DNS is the one place jailmachine is measurably ahead of
> both competitors, and the UDP ceiling is the one place its network is
> measurably behind Linux.

---

## Port publishing

| Measurement | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|
| `run -p` → first successful `curl` | **2.29 s** | 0.20 s | 0.24 s |

The gap is architectural, not incidental. Docker and podman publish a port as
part of starting the container; jailmachine's forwarder **reconciles** —
it watches podman events (debounced 300 ms) plus a 30 s timer, then converges
gvproxy's mapping table onto the guest's container state
([ADR 0004](adr/0004-networking-as-a-provider-with-reconciled-port-publishing.md)).
That buys crash-safety and per-mapping error reporting, and it costs a second
or two of lag on every publish. Scripts must retry:
`curl --retry 10 --retry-connrefused`.

What jailmachine offers in exchange:

| | |
|---|---|
| `jm ports` | Lists every mapping, where it binds, and the error per mapping (host port busy, forwarder down) — there is no Docker Desktop or podman machine equivalent |
| `--publish-addr` | The default host bind address is a **machine property**, not ambient shell state; `0.0.0.0` like docker, `127.0.0.1` to keep containers off your LAN |
| `-p 127.0.0.1:8080:80` | Works, via a `rdr` rule in the guest's own `pf` anchor |

> **Interpretation:** slower to converge, better to debug. The 2.29 s is a
> real cost in test suites that start a container and immediately connect.

---

## Build

| # | Measurement | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|---|
| 4 | Cold-cache pull of `debian:trixie-slim` (median of **six**) | 6.35 s | 3.55 s | 2.09 s |
| 5 | Build, 5-step Containerfile, `--no-cache` | 8.45 s | 5.49 s | 6.56 s |
| 5b | The same file unmodified | **fails** (apt, 3.2–3.6 s each attempt) | 5.71 s | 5.53 s |

Row 5 is the like-for-like row: all three ran the same file with
`-o APT::Cache-Start=200000000` added, because without it the build fails on
jailmachine (**FreeBSD**: no growable `mremap`, so APT's dynamic cache cannot
grow). Row 5b is the file as originally written, and is a pass/fail rather
than a timing. Note that this compares **different builders** — Docker 29 uses
BuildKit, both podman stacks use buildah, and BuildKit parallelises where
buildah does not — and that `apt-get install curl` reaches the network, so the
row carries mirror variance.

Three build-time limitations, jailmachine only:

| Limitation | Measured | Whose |
|---|---|---|
| `jdocker build` fails out of the box on this Mac: `no image found in image index for architecture "arm64", variant "", OS "freebsd"` | buildx tries to boot `moby/buildkit` inside the FreeBSD engine. **`DOCKER_BUILDKIT=0 jdocker build` succeeds**, as does `jpodman build --os=linux` | **upstream tooling** (no FreeBSD buildkit image) plus **ours** (`jm` exports `DOCKER_DEFAULT_PLATFORM` for `jdocker` but not `DOCKER_BUILDKIT` — a one-line fix in `internal/cli/docker.go`). Severity is install-dependent — it turns on which buildx builder is active — but it reproduced here with the stock Docker Desktop arrangement |
| `RUN --mount=type=cache` | `Error: … cache mounts not supported on freebsd` | **podman-on-FreeBSD** (buildah) |
| `RUN --mount=type=secret` | Works | — |

> **Interpretation:** builds are 30–55 % slower than the alternatives, which is
> tolerable; the `jdocker build` failure is not, and it is ours to fix.

---

## Orchestration: compose and `kube play`

**No multi-container stack was run in this comparison.** What is measured is
the supervision behaviour underneath one, and it is the strongest reason not
to run a stack on jailmachine yet. One row was measured on jailmachine
afterwards, while the tutorials were written, and is marked as not
re-measured on the other two: container-to-container name resolution.

| | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|
| `docker compose` / `podman compose` | Works through `jdocker compose` / `jpodman compose` — the provider runs **on the Mac** and drives the guest's podman over `DOCKER_HOST`. Nothing compose-shaped is installed in the guest, by choice rather than necessity: `py312-podman-compose` 1.5.0 *is* packaged for FreeBSD, but jm's compose story is host-side | Native | Native, bundled |
| **Container name resolution between services** | **Does not work.** `nc: bad address 'redis'`; `podman network inspect podman --format '{{.DNSEnabled}}'` prints `false`. The guest runs podman's CNI backend — `netavark` is not packaged for FreeBSD, nor is the CNI `dnsname` plugin — so the default `depends_on` + `host: redis` shape of most compose files needs rewriting: a Pod (`localhost`), `network_mode: "service:<name>"`, or `extra_hosts`. Podman-on-FreeBSD's gap, not jm's ([#5](https://github.com/gabrielbelli/jailmachine/issues/5)) | Works — netavark + aardvark-dns, on by default (not re-measured here) | Works — the embedded DNS server on any user-defined network (not re-measured here) |
| `podman kube play` | Works — the route podman implements itself, and the FreeBSD-native answer | Works | n/a |
| Healthchecks | **Never fire.** `--health-cmd true --health-interval 2s`: after 20 s, status still `starting`, 0 log entries. `podman healthcheck run <name>` by hand returns `healthy` immediately | Work | Work |
| Restart policies | **They work**: `--restart=always` on a container exiting 3 every 2 s took `RestartCount` 2→4→6→9→10 over 25 s; `--restart=on-failure:3` stopped at exactly 3; across `jm stop` + `jm start`, `always` came back `Up` while a `no` control stayed `Exited (143)`. The docs said otherwise and have been corrected | Work | Work |
| `--memory`, `--cpus`, `--cpuset-cpus`, `--cpu-shares` | **Accepted and discarded.** `--memory=64m` still allocated 400 MiB; the same CPU loop took 2.95 s unrestricted and 2.91 s at `--cpus=0.1`. `podman inspect` records `Memory=0 NanoCpus=0` | Enforced | Enforced |
| `--privileged`, `--cap-add`, `--cap-drop`, `--security-opt`, `--uidmap`, `:z`/`:Z` | **Accepted and discarded.** Under `--privileged`: `mknod` → `Operation not permitted`, `/dev` byte-identical to an unprivileged container. `--uidmap 0:100000:5000` still ran as uid 0 | Enforced | Enforced |
| Rootless | Refused explicitly: `rootless mode is not supported on FreeBSD` | Default | n/a |
| Swarm | `Podman does not support service: /v1.44/swarm/init` | Same | Supported |
| Bundled Kubernetes | No equivalent exists | No equivalent exists | Yes |

Attribution: healthchecks are **podman-on-FreeBSD** (no systemd, so no
transient timer units); resource limits are **podman-on-FreeBSD** (`ocijail`
applies nothing) with **FreeBSD** contributing (`kern.racct.enable: 0`, kernel
built `RACCT_DEFAULT_TO_DISABLED`); capabilities and user namespaces are
**FreeBSD** (a jail is the security boundary and denies these regardless).
[#3](https://github.com/gabrielbelli/jailmachine/issues/3) is half right and
still needs amending: its healthcheck claim holds, its restart claim does not.
The pages have been fixed; the issue has not.

> **Interpretation:** the dangerous part is **silent acceptance**. A hardened
> compose file that sets `--cap-drop=ALL`, `--memory` and a `healthcheck:` will
> start without a single warning on jailmachine and enforce none of them. Do
> not port a production stack here expecting its guarantees to travel.

---

## Jails and native FreeBSD images

This is the axis on which the comparison stops being a comparison: **Docker
Desktop and podman machine cannot run a FreeBSD kernel at all**, so there is
no row to fill in for them.

| | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|
| Native FreeBSD OCI images | **Works, no flag.** `ghcr.io/freebsd/freebsd-runtime:15.1` and `:14.3`, `dougrabson/freebsd15-minimal` and the project's own demo images all ran; `security.jail.jailed=1` inside | Impossible | Impossible |
| Build a FreeBSD image (`pkg install` in a `Containerfile`) | Works | Impossible | Impossible |
| `bastille` jails | Installed and configured in the guest (ZFS, `bastille0` loopback, NAT through `pf`), driven with `jm ssh -- bastille …` (`bastille list all` — `-a` is deprecated in 1.4.4; `bastille destroy -a -y <jail>` is the non-interactive removal, not `-f`). **Not exercised in this comparison** | Impossible | Impossible |
| Host-side jail management (`jm jail …`) | Not implemented, deliberately out of MVP scope ([ADR 0006](adr/0006-scope-boundaries.md)) | — | — |

Two honest qualifications. First, jails were **not run** during these
measurements — what was verified is that the guest ships and configures
`bastille`, not that a jail booted; treat the jail row as a shipped feature,
not a measured one. Second, if all you need is a FreeBSD machine and not the
podman half, a plain FreeBSD VM under QEMU or a rented FreeBSD box does the
same job without jailmachine.

---

## Lifecycle ergonomics

| | jailmachine | podman machine | Docker Desktop |
|---|---|---|---|
| Shape | One Go binary, plus `jpodman` / `jdocker` symlinks to it | One binary, part of podman | Application bundle, GUI, background services |
| Host daemon | None. `jm start` leaves QEMU, gvproxy, the forwarder and the resolver detached and exits | None | Yes (12 processes idle) |
| Login agent | **Deliberately none.** `jpodman` and `jdocker` start a stopped machine on demand instead; `JM_AUTOSTART=0` opts out | None | Optional, on by default |
| Diagnostics | `jm doctor` — 23 checks on the user's machine (tools, HVF, firmware, state root, share parity, resolver parity, per-machine); every failure prints a fix. `jm ports` explains each mapping; five named log files per machine | `podman machine inspect`, `podman info` | GUI dashboard, `docker desktop` CLI |
| Uninstall | `rm -rf ~/.jailmachine` is a complete uninstall ([ADR 0005](adr/0005-state-and-lifecycle-model.md)) | `podman machine rm` — **but it kept 0.87 GiB of image cache afterwards** | Uninstaller |
| Multiple machines | Yes, each with its own SSH port and podman connections | Yes | One VM |
| Default-connection hygiene | `jm start` registers a connection and **leaves your default alone** (`--set-default` opts in) | `podman machine rm -f` silently promoted an unrelated connection to default during this benchmark, displacing `jailmachine` — **upstream tooling** | Owns its own context |
| GUI, extensions, image scanning, Dev Environments | None, and none planned | None | Yes |

> **Interpretation:** jailmachine's lifecycle model is the part that is
> genuinely finished — idempotent, crash-tolerant, one directory, one binary,
> nothing running at login. It is also the part with the least competitive
> pressure: podman machine is just as clean and starts three times faster.

---

## Where jailmachine wins

| Win | Evidence |
|---|---|
| **Native FreeBSD OCI images and jails on a Mac** | Six FreeBSD images ran, including builds; the other two cannot do this at any speed. This is the entire reason the project exists |
| **DNS that matches the Mac** | The only one of the three that turns a host `127.0.0.1` answer into an address a container can reach, and the only one that refuses to echo a `0.0.0.0` sinkhole. Hosts entries and mDNS names all resolve inside a container |
| **One binary, no daemon, no login agent** | 6 host processes and 196 MiB idle against Docker Desktop's 12 and 1695 MiB; `rm -rf ~/.jailmachine` is a complete uninstall |
| **Honest, inspectable plumbing** | `jm ports` reports a per-mapping error; `jm doctor` checks every host tool and every machine, with a fix per failure; `--publish-addr` is a machine property, so what binds your LAN is visible in `jm inspect` rather than implied by the shell that booted the machine |
| **Bind mounts at the identity path** | `-v ~/code:/app` and `-v ~/code:"$HOME/code"` both work from any directory, with no `/host_mnt` prefix and no argument rewriting — slow, but correct |

## Where it loses

| Loss | Size of it | Whose |
|---|---|---|
| **`linux/amd64` images do not run** | `pull` succeeds, `run` → `Exec format error`. No workaround found; `qemu-user-static` + `binmiscctl` in the guest is the only plausible route and was not tested | **FreeBSD** (no `binfmt_misc`) and **Apple** (Rosetta for Linux ships only to Linux guests, over Virtualization.framework and virtiofs — jailmachine is QEMU and FreeBSD, so it is out twice) |
| **File sharing is 7–18× slower** | 63 MB/s write and 85 MB/s read against 466 MB/s; 1000 small files in 4.26 s against 0.36–0.58 s; a shallow clone in 4.72 s against 0.26–0.31 s | **upstream tooling / FreeBSD** — virtio-9p plus FreeBSD `p9fs`, with no virtiofs driver to switch to. [#4](https://github.com/gabrielbelli/jailmachine/issues/4) |
| **File watchers do not work on a share** | The watch cannot even be created: `inotify_add_watch` → `Bad file descriptor`. Polling watchers work | **FreeBSD** — the Linuxulator's inotify does not work over `p9fs`; it works on a volume and on the container's own filesystem. [#4](https://github.com/gabrielbelli/jailmachine/issues/4) |
| **Healthchecks never fire** | Status sits at `starting` with 0 log entries; run them by hand or from cron in the guest | **podman-on-FreeBSD** — no systemd, so no transient timer units. [#3](https://github.com/gabrielbelli/jailmachine/issues/3) |
| **Resource limits and security flags are accepted and ignored** | `--memory=64m` allocated 400 MiB; `--cpus=0.1` cost 1 % of runtime; `--privileged` grants nothing; `--uidmap` still runs as uid 0 | **podman-on-FreeBSD** (no cgroups, `ocijail` applies nothing) and **FreeBSD** (no capabilities, no user namespaces, RACCT disabled). Silent acceptance is the dangerous part |
| **PostgreSQL does not run, at any version** | ≥ 14 needs `signalfd`; ≤ 13 needs SysV shared memory | **FreeBSD** (`signalfd4` → `ENOSYS`) and **podman-on-FreeBSD** (`ocijail` hard-disables SysV IPC). 14–16 were **not tested**; that they fail is an inference from PostgreSQL's use of `signalfd` since v14 |
| **`node:22-alpine` hangs, and `apt-get` fails** | `node --version` is the only thing that works; APT dies with `Dynamic MMap ran out of room` | **FreeBSD** — one missing capability, `linux_mremap` cannot grow a mapping. Use `node:22-bookworm-slim`, and `-o APT::Cache-Start=200000000` for APT. **Not musl's fault**: alpine busybox, `python:3-alpine`, `apk add` and `pip install` all work |
| **Slower at every engine round trip** | hello-world 1.87 s vs 0.21 / 0.30 s; pull 6.35 s vs 3.55 / 2.09 s; first curl after `-p` 2.29 s vs 0.20 / 0.24 s | Mostly **ours** by design: the hello-world gap is the registry check `--os=linux` forces, and the port gap is the reconciler's convergence lag |
| **Slow to start, and enormous on disk** | Warm start 36.7 s vs 10.4 s; `init` writes 46.87 GiB for 3.17 GiB of content and takes 59–113 s from a cached image | **ours** — `internal/image/sparse.go` is not punching holes reliably; the boot time is QEMU plus a full FreeBSD boot |
| **`jdocker build` fails out of the box on a Docker Desktop Mac** | `DOCKER_BUILDKIT=0` fixes it | **upstream tooling** (no FreeBSD buildkit image) plus **ours** (the wrapper does not set the variable) |
| **Container IPs are not routable from the Mac** | `curl` to `10.88.0.36` times out from the host | **ours** (gvproxy was chosen) and **upstream tooling** (it is a userspace NAT). Docker Desktop on macOS behaves the same way |
| **macOS on Apple Silicon only** | No Intel, Linux or Windows host backend | **ours** (scope) |

## What was not measured

- Docker Desktop's cold start, warm start and stop — it was running the user's
  containers and restarting it was out of bounds.
- Any multi-container compose stack, on any of the three.
- `bastille` jails actually running.
- Lima, Colima and OrbStack — see
  [the paragraph above](#not-measured-here-lima-colima-and-orbstack).
- A split-horizon or VPN resolver, because this Mac had no `/etc/resolver` entries.
- PostgreSQL 14, 15 and 16; Elasticsearch, OpenSearch and Kafka (the machine has
  4 GiB of RAM); systemd-in-container images (the build to produce one was
  blocked by the APT bug); GPU workloads; anything needing `--privileged` to
  do something real.
- Docker Desktop's surface beyond the engine — extensions, bundled Kubernetes,
  Dev Environments, `docker scout`. The honest framing is "no equivalent
  exists", not "it fails".
- Every compatibility row is a **single run**: no repetitions, no timings, no
  flake analysis.

## Sources

- [README](../README.md) — what jailmachine claims to do
- [docs/USAGE.md](USAGE.md) — the flags and the verified Docker Hub matrix
- [docs/LIMITATIONS.md](LIMITATIONS.md) — every limitation, measured and attributed
- [docs/TROUBLESHOOTING.md](TROUBLESHOOTING.md) — symptoms and workarounds
- [docs/ARCHITECTURE.md](ARCHITECTURE.md), [docs/adr/](adr/) — why it is shaped this way
- [#2](https://github.com/gabrielbelli/jailmachine/issues/2) UDP and MTU ·
  [#3](https://github.com/gabrielbelli/jailmachine/issues/3) healthchecks and restart ·
  [#4](https://github.com/gabrielbelli/jailmachine/issues/4) 9p speed and inotify
