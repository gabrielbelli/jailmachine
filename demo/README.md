<!-- linked from README.md -->
# jailmachine demo images

Five small container images that show, in about five minutes, what
`jailmachine` actually gives you: a **FreeBSD 15.1 kernel** on your Mac that
runs **native FreeBSD containers** and **unmodified Linux containers** side by
side, from the same `podman` client.

Every image runs with no arguments, prints something worth reading, and exits
0. Published as `ghcr.io/gabrielbelli/jm-demo-<name>`.

| In this directory | What it is |
|---|---|
| [The images](#the-images) | The five demo images and the one-liner that runs each |
| [The five-minute demo script](#the-five-minute-demo-script) | Copy-paste tour, in order |
| [The nginx finding](#the-nginx-finding) | Why stock `nginx:alpine` dies, and the one line that fixes it |
| [`hub-matrix.sh`](#the-docker-hub-matrix) | Re-runs the whole Docker Hub compatibility matrix from `docs/USAGE.md` |
| [How these images are published](#how-these-images-are-published) | `make -C demo build test push`, and why FreeBSD `:latest` is manual |

## The images

| Image | Base | What it proves | Run it |
|---|---|---|---|
| `jm-demo-freebsd` | `ghcr.io/freebsd/freebsd-runtime:15.1` | The kernel and userland really are FreeBSD, and a container here **is a jail** (`security.jail.jailed=1`). Beastie included. | `jpodman run --rm ghcr.io/gabrielbelli/jm-demo-freebsd` |
| `jm-demo-linuxulator` | `docker.io/library/alpine:3.24` | A **stock, unpatched Alpine** running on FreeBSD's Linux ABI — with every piece of evidence annotated in one or two words. | `jpodman run --rm --os=linux ghcr.io/gabrielbelli/jm-demo-linuxulator` |
| `jm-demo-nginx-linuxulator` | `docker.io/library/nginx:1.31-alpine` | A **real Docker Hub server image serving traffic** under the Linuxulator. One config line makes it work; see [the finding](#the-nginx-finding). | `jpodman run -d --os=linux -p 8080:80 ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator` |
| `jm-demo-nginx-native` | `ghcr.io/freebsd/freebsd-runtime:15.1` + `pkg install nginx` | The **same site, same URLs, native FreeBSD nginx** on `kqueue`. Put it next to the one above and compare. | `jpodman run -d -p 8081:80 ghcr.io/gabrielbelli/jm-demo-nginx-native` |
| `jm-demo-volume` | `ghcr.io/freebsd/freebsd-runtime:15.1` | An engine-managed volume lives on the **guest's ZFS pool inside the VM**, at native speed — the fast path for build output and databases, unlike a 9p share (`-v /Users/you/src:/app` does work, it is just slow). | `jpodman run --rm -v jm-demo-data:/data ghcr.io/gabrielbelli/jm-demo-volume` |

Sizes (uncompressed, on the guest): 9 MB, 32 MB, 32 MB, 64 MB, 67 MB.

> **Pass `--os=linux` for the Linux images.** The jailmachine guest is a
> FreeBSD host, so `podman` there defaults to `freebsd/arm64`. Any command that
> has to pick a manifest — `build`, `pull`, and therefore the first `run` —
> fails without it. Once the image is local, `run` works anyway but prints
> `WARNING: image platform (linux/arm64/v8) does not match the expected
> platform (freebsd/arm64)`; pass the flag and keep the output clean. The three
> FreeBSD images must *not* have it.

## The five-minute demo script

Copy-paste, in order. Assumes `jm start` has already finished.

```bash
# 0. Prove the client is talking to a FreeBSD machine, not Docker Desktop.
jpodman info --format '{{.Host.OS}}/{{.Host.Arch}}  {{.Host.Distribution.Distribution}}'
#   -> freebsd/arm64  freebsd
```

```bash
# 1. "This is really FreeBSD."  (~2 s)
jpodman run --rm ghcr.io/gabrielbelli/jm-demo-freebsd
```
Point at three lines: `uname` says FreeBSD, `jailed? yes` — on FreeBSD a
container *is* a jail, there is no separate container runtime — and
`linux ABI 5.15.0`, which is the next demo.

```bash
# 2. "And this is Linux, on that same kernel."  (~2 s)
jpodman run --rm --os=linux ghcr.io/gabrielbelli/jm-demo-linuxulator
```
The two lines that land: `uname -a` reads
`Linux ... 5.15.0 FreeBSD 15.1-RELEASE-p2 ... GENERIC aarch64`, and
`/proc/version` says it was built by `des@freebsd.org` with FreeBSD Clang.
No Linux kernel exists anywhere in this stack.

```bash
# 3. A real Docker Hub server image, serving real traffic.
jpodman run -d --name nginx-linux --os=linux -p 8080:80 \
    ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator
curl -s --retry 10 --retry-connrefused localhost:8080/whoami.txt
open http://localhost:8080          # a browser is more fun
```

```bash
# 4. The same site, native.
jpodman run -d --name nginx-native -p 8081:80 \
    ghcr.io/gabrielbelli/jm-demo-nginx-native
curl -s --retry 10 --retry-connrefused localhost:8081/whoami.txt
open http://localhost:8081
```
Two tabs, identical pages. One is an `x86`/`aarch64` **Linux** ELF binary, the
other a **FreeBSD** ELF binary. Same kernel, same CPU, same 4 cores.

```bash
# 5. The same image again, this time from a Kubernetes manifest.
cat > /tmp/jm-demo-pod.yaml <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: jm-demo-web
spec:
  containers:
    - name: web
      image: ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator
      imagePullPolicy: IfNotPresent   # keep the image pulled with --os=linux above
      ports:
        - containerPort: 80
          hostPort: 8082
EOF
jpodman kube play /tmp/jm-demo-pod.yaml
curl -s --retry 10 --retry-connrefused localhost:8082/whoami.txt
```
`podman kube play` is podman's **own** orchestrator: nothing is installed in
the guest for it, and no compose provider runs on the Mac — which is why it
is the FreeBSD-native answer to a compose file. The manifest's `hostPort`
goes through the same forwarder as `-p`, so `jm ports` lists it beside the
other two. The three orchestration routes are in
[`docs/USAGE.md`](../docs/USAGE.md#compose-and-kubernetes-yaml).

```bash
# 6. Where did the data go?
jpodman run --rm -v jm-demo-data:/data ghcr.io/gabrielbelli/jm-demo-volume
jpodman run --rm -v jm-demo-data:/data ghcr.io/gabrielbelli/jm-demo-volume   # log grew
jm ssh -- ls -l /var/db/containers/storage/volumes/jm-demo-data/_data
```
The last command is the punchline: the file is on the guest's ZFS pool, where
writes run at native ZFS speed rather than across a 9p share, and it outlives
every container that used it. `jm ssh` is how you reach it directly.

```bash
# 7. Tidy up.
jpodman kube down /tmp/jm-demo-pod.yaml
jpodman rm -f nginx-linux nginx-native
jpodman volume rm jm-demo-data
```

### Is the Linuxulator slower?

Not measurably, for this workload. Measured inside the guest (removing the
`gvproxy` port-forward and `curl` startup from the numbers), 1000 sequential
HTTP requests to `/healthz`:

| Server | 1000 requests | per request |
|---|---|---|
| `jm-demo-nginx-linuxulator` (Linux nginx 1.31.4) | 1.55 s | 1.55 ms |
| `jm-demo-nginx-native` (FreeBSD nginx 1.30.4) | 1.56 s | 1.56 ms |
| *control: 1000 × `/usr/bin/true`* | *0.46 s* | *0.46 ms* |

That is the expected result. The Linuxulator is a second **syscall table**,
not emulation: the CPU executes the same aarch64 instructions either way, and
only the kernel entry path differs. Nearly half of each measurement above is
process spawn, not HTTP.

## The nginx finding

Stock `nginx:alpine` does **not** run under the Linuxulator. Every worker dies
at startup:

```
[alert] 8231#8231: epoll_ctl(1, 6) failed (22: Invalid argument)
[alert] 8154#8154: worker process 8231 exited with fatal code 2 and cannot be respawned
```

**Cause.** Since 1.11.3 nginx defaults to `accept_mutex off`, and with more
than one worker it registers the listening socket with `EPOLLEXCLUSIVE` so the
workers do not thundering-herd on `accept()`. FreeBSD's `linux_epoll`
emulation does not implement `EPOLLEXCLUSIVE` and returns `EINVAL`, which
nginx treats as fatal.

**Fix — one line.** `accept_mutex on;` in the `events` block. nginx then
serialises `accept()` itself and never sets the flag.

Measured on this machine (FreeBSD 15.1-RELEASE-p2 arm64, nginx 1.31.4), same
config otherwise:

| `worker_processes` | `accept_mutex` | Result |
|---|---|---|
| 4 (`auto`) | `off` (default) | workers die, connection refused |
| 4 (`auto`) | **`on`** | **HTTP 200, all 4 workers alive** |
| 1 | `off` | HTTP 200 — `EPOLLEXCLUSIVE` is only used when workers > 1 |
| 2 | `off` | workers die |

**`use poll;` and `use select;` do not work**, and it is worth knowing why:
the official nginx images are built on Linux with `epoll` available, and
nginx's `configure` then omits the `poll` and `select` event modules
altogether. You get a config error, not a fallback:

```
[emerg] invalid event type "poll" in /etc/nginx/nginx.conf:4
```

**One harmless leftover.** Every worker still logs, once, at startup:

```
[emerg] 12097#12097: io_setup() failed (38: Function not implemented)
```

Linux AIO is not implemented by the Linuxulator. nginx probes for it
unconditionally in its epoll module, logs this, disables file AIO and carries
on serving. It cannot be configured away, and `aio off;` does not silence it.

`demo/nginx-linuxulator/nginx.conf` carries this whole story in comments.

## The Docker Hub matrix

The five images above are hand-made. `demo/hub-matrix.sh` does the opposite:
it pulls **unmodified images straight from Docker Hub** and reports which
ones work.

```bash
jm start
demo/hub-matrix.sh          # prints an aligned table; exit 0 if reality matches the docs
```

It covers `alpine`, `debian`, `ubuntu`, `python`, `golang`, `hello-world`,
`caddy`, `postgres`, `redis` and `nginx` on the Linux side (all with
`--os=linux`), the two `dougrabson/freebsd*-minimal` images and the two
`ghcr.io/freebsd/freebsd-runtime` tags on the FreeBSD side (no flag), and
the two workarounds and one known-bad image documented in
[`docs/USAGE.md`](../docs/USAGE.md#docker-hub-compatibility-verified):
`nginx` needs `accept_mutex on;`, `redis-server` needs
`--ignore-warnings ARM64-COW-BUG`, and `node:22-alpine` runs only
`node --version` — every other invocation hangs. Each container is run under
`timeout 120`.

## Known limits

- **Bind mounts work, but over 9p.** FreeBSD has no virtiofs driver, so jm
  shares host directories with `virtio-9p` at the *same absolute path* —
  `-v /Users/you/src:/app` works, from any directory. It is slow (~70 MB/s,
  and metadata far worse), `utimes` is a silent no-op and an `inotify` watch
  cannot be created on a shared path at all (`inotify_add_watch` →
  `Bad file descriptor`; it works on a volume and on the container's own
  filesystem), so keep build output in a named volume on the guest's ZFS
  pool and use a polling watcher (`CHOKIDAR_USEPOLLING=1`,
  `nodemon --legacy-watch`, `--watch.usePolling`). A file a container creates
  shows on the Mac as `0600`, its real mode in a `user.virtfs.mode` xattr —
  the price of the `mapped-xattr` security model that makes root in a
  container work. `jm inspect` lists what is shared;
  `jm set --mount/--unmount/--no-mounts` changes it, on a stopped machine.
- **`--os=linux` on every Linux image**, for `build` and `run` alike.
- **Not every Linux image works.** The Linuxulator implements most of the
  Linux syscall surface, not all of it — the missing `EPOLLEXCLUSIVE` is
  what this demo runs into, and `docker.io/node` hangs on anything past
  `node --version` (use `node:22-bookworm-slim`). The
  verified matrix is in
  [`docs/USAGE.md`](../docs/USAGE.md#docker-hub-compatibility-verified) and
  `hub-matrix.sh` re-runs it. Expect to meet more gaps with anything that
  reaches deep into `/sys`, cgroups, or `io_uring`.
- **FreeBSD `:latest` tags are arm64.** On an amd64 FreeBSD host, pull the
  `:freebsd-amd64` tag instead.

## How these images are published

Publishing is **manual, from a Mac running jailmachine**, because GitHub has no
FreeBSD runner on any architecture and its hosted runners have no nested
virtualisation, so a FreeBSD guest in CI could only run under slow emulation:

```bash
jm start
gh auth token | jpodman login ghcr.io -u <your-github-user> --password-stdin
make -C demo build test push        # arm64 images, tagged :latest and :YYYYMMDD
```

Images land as `ghcr.io/gabrielbelli/jm-demo-<name>`; new packages start
private, so flip each one to Public once under
`https://github.com/users/<user>/packages/container/<name>/settings`.

CI (`.github/workflows/demo-images.yml`) is a signal-only check: it builds the
two Linux images for `linux/amd64` and `linux/arm64` on native runners and
smoke-tests them, and pushes nothing. When FreeBSD arm64/amd64 runners become
available, publishing can move there.
