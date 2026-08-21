<!-- linked from docs/tutorials/README.md -->
# 1. Your first container

Install `jailmachine`, create a machine, run a Linux image and a native
FreeBSD image, publish a port and reach it from the Mac, look inside the
guest, and shut it down again.

| | |
|---|---|
| You need | macOS 14+ on Apple Silicon, about 10 GB free disk, about 4 GB free RAM |
| Time | 3–5 minutes of typing, plus roughly 2 minutes of downloading and booting |
| You end with | A working machine, then a stopped one — and the option to delete it entirely |

Nothing here assumes Docker Desktop, and nothing here touches it if you have
it: `jm` never repoints a podman connection or a docker context you already
had.

---

## Step 1 — install

Three paths. They differ only in what gets installed for you.

| Path | Installs `qemu` + `podman` for you | Creates `jpodman` / `jdocker` | Gatekeeper handled |
|---|---|---|---|
| Homebrew cask | Yes | Yes | Yes |
| `go install` | No | No — link them yourself | May need `xattr` |
| From source | No | Yes | Not needed (locally built) |

**Homebrew cask** — the short path:

```bash
brew install --cask gabrielbelli/tap/jailmachine
```

**`go install`** — you arrange the dependencies and the two symlinks:

```bash
brew install qemu podman
go install github.com/gabrielbelli/jailmachine/cmd/jm@latest
export PATH="$(go env GOPATH)/bin:$PATH"
ln -sf "$(go env GOPATH)/bin/jm" "$(go env GOPATH)/bin/jpodman"
ln -sf "$(go env GOPATH)/bin/jm" "$(go env GOPATH)/bin/jdocker"
```

**From source** — `PREFIX` defaults to `/opt/homebrew`:

```bash
git clone https://github.com/gabrielbelli/jailmachine
cd jailmachine
make install            # or: make install PREFIX="$HOME/.local"
```

> `jm` decides it is being run as a wrapper from the **name of the binary**,
> so the two symlinks must be called exactly `jpodman` and `jdocker`.

`qemu` provides the hypervisor (through Hypervisor.framework) and the EDK2
firmware; `podman` provides both the host client and `gvproxy`, the userspace
network stack. The docker CLI is optional — install it only if you want
`jdocker` or `docker compose` (see [tutorial 3](03-a-stack-with-compose-and-kube.md)).

Full install detail, including uninstalling, is in
[docs/INSTALL.md](../INSTALL.md).

---

## Step 2 — `jm init`

`init` creates the machine on disk. It generates an SSH key, downloads the
prebaked guest image and checks its SHA256, expands it sparsely into
`disk.raw`, and writes the first-boot seed. It boots nothing.

```bash
jm init
```

```text
==> share: /Users/you (rw)
==> share: /Volumes (rw)
==> share: /private/tmp (rw)
==> share: /var/folders/5l/x_34hg657n93c50c02sbjb040000gn (rw)
==> ssh-key: generating /Users/you/.jailmachine/machines/jailmachine/ssh/id_ed25519
==> image: fetching prebaked:15.1.0
downloading https://github.com/gabrielbelli/jailmachine/releases/download/guest-15.1.0/jailmachine-guest-15.1.0-freebsd15.1-arm64-zfs.raw.zst
verifying SHA256
decompressing (sparse)
image ready: /Users/you/.jailmachine/machines/jailmachine/disk.raw
==> seed: writing first-boot seed /Users/you/.jailmachine/machines/jailmachine/seed.iso
==> done: created jailmachine (4 cpus, 4096 MiB, 64 GiB). Next: jm start jailmachine
```

Expect **60–115 s**. The 800 MiB download is only part of it; writing
`disk.raw` out is the rest.

Two things worth noticing in that output before you move on:

- The four `share:` lines are host directories that will appear **inside the
  guest at the same absolute path**. That is [tutorial 2](02-develop-on-a-shared-folder.md).
- `disk.raw` is created sparse at 64 GiB and only occupies what the guest
  actually writes — a fresh machine is about 4 GiB on disk.

Everything lives under `~/.jailmachine/machines/jailmachine/`. Nothing is
written anywhere else, so deleting that directory is a complete uninstall of
state.

> **Want it somewhere else, or several machines?** `--state-root` (or
> `$JM_HOME`) moves the lot, and `jm init --ssh-port 2223 dev` makes a second
> machine — each machine needs its own SSH port.

---

## Step 3 — `jm start`

```bash
jm start
```

```text
==> network: starting gvproxy networking
==> backend: booting jailmachine (4 cpus, 4096 MiB, ssh on 127.0.0.1:2222)
==> ssh: waiting for sshd
...........
==> provision: waiting for /var/db/jm-provisioned
==> dns: pointing the guest at the host resolver on 192.168.127.254:53984 (search mygateway)
==> connect: forwarding /var/folders/.../jm-c3771d1ab4c4.sock to the guest podman socket
==> connect: configuring podman connection "jailmachine"
==> connect: podman connection "jailmachine-sock" -> unix:///var/folders/.../jm-c3771d1ab4c4.sock
==> forwarder: starting the port forwarder, publishing on 0.0.0.0 (log: /Users/you/.jailmachine/machines/jailmachine/forwarder.log)
==> ready: try 'jpodman run --rm --os=linux docker.io/alpine echo hi'
```

Measured timings, both on a **busy** Mac (another VM and Docker Desktop
running throughout):

| | This run | On an idle Mac |
|---|---|---|
| First boot, prebaked image | 36 s | about 22 s |
| Later starts | 37 s | 12–25 s |
| `jm stop` | 13 s | — |

Every stage line names something you can go and read if it fails: `qemu.log`
and `console.log` for the hypervisor, `gvproxy.log` for the network,
`forwarder.log` for publishing, and `/var/log/jm-provision.log` **inside the
guest** for provisioning.

If a stage does fail, the error says which one, and `jm console -f` follows
the guest's serial console live.

---

## Step 4 — run a Linux image

```bash
jpodman run --rm --os=linux docker.io/alpine echo hi
```

```text
hi
```

`jpodman` is the host's `podman`, aimed at this machine. It does not touch
your default podman connection, and it **starts a stopped machine for you**:

```bash
jm stop
jpodman ps
```

```text
starting jailmachine "jailmachine"...
CONTAINER ID  IMAGE       COMMAND     CREATED     STATUS      PORTS       NAMES
```

`JM_AUTOSTART=0`, or `--no-autostart` as the first argument, makes it fail
instead of booting.

### Why `--os=linux`, and why FreeBSD images need nothing

This is the one piece of jailmachine you have to hold in your head, so here
it is properly.

An image reference on a registry usually points at a **manifest list**: one
entry per `os/arch` pair. When podman pulls, it picks the entry matching the
platform of the **engine** — and this engine runs on FreeBSD, so the default
selector is `freebsd/arm64`. Docker Hub's Linux images have no such entry:

```bash
jpodman run --rm docker.io/library/hello-world
```

```text
Trying to pull docker.io/library/hello-world:latest...
Error: unable to copy from source docker://hello-world:latest: choosing an image
from manifest list docker://hello-world:latest: no image found in image index for
architecture "arm64", variant "", OS "freebsd"
```

`--os=linux` overrides the OS half of that selector. That is all it does. The
image is then run through FreeBSD's **Linuxulator** — a second syscall table
in the FreeBSD kernel, not emulation — so the same aarch64 instructions
execute either way.

Native FreeBSD images publish a `freebsd/arm64` entry, which is exactly what
the default selector already asks for, so they need no flag at all:

```bash
jpodman run --rm ghcr.io/freebsd/freebsd-runtime:15.1 uname -srm
```

```text
FreeBSD 15.1-RELEASE-p2 arm64
```

Three consequences worth knowing:

| Situation | What happens |
|---|---|
| The Linux image is already in the guest's store | A bare `run` works, but warns: `WARNING: image platform (linux/arm64/v8) does not match the expected platform (freebsd/arm64)`. Pass the flag and keep the output clean |
| You use `jdocker` instead | The docker CLI has no `--os` flag, so the wrapper defaults `DOCKER_DEFAULT_PLATFORM=linux/arm64`. `jdocker run --rm docker.io/alpine echo hi` just works; `DOCKER_DEFAULT_PLATFORM= jdocker run …` opts back out for a FreeBSD image |
| The image is `linux/amd64` only | It **pulls** and then refuses to execute: `ocijail: error executing container command: Exec format error`. There is no emulation path on this platform — pull or build the arm64 variant |

Almost everything on Docker Hub works under `--os=linux`. The verified matrix,
the two images that need one flag or one config line (`redis`, `nginx`) and
the one that does not work at all (`node:22-alpine`) are in
[docs/USAGE.md](../USAGE.md#docker-hub-compatibility-verified) and
[docs/LIMITATIONS.md](../LIMITATIONS.md#the-linuxulator).

---

## Step 5 — publish a port and reach it from the Mac

```bash
jpodman run -d --rm --os=linux -p 8080:80 --name web docker.io/busybox \
  sh -c 'echo hello from the FreeBSD VM > /tmp/index.html && httpd -f -p 80 -h /tmp'
curl -s --retry 10 --retry-connrefused http://localhost:8080/
```

```text
hello from the FreeBSD VM
```

The `--retry-connrefused` is not decoration. Publishing here is **reconciled,
not requested**: a forwarder started by `jm start` watches the guest's
container events and converges gvproxy's mapping table onto them, which takes
a second or two after the container starts. A bare `curl` fired immediately
usually gets one connection refused first.

`jm ports` is the thing to run when a port does not answer. It lists every
mapping the forwarder owns, and the error per mapping:

```bash
jm ports
```

```text
# publishing on 0.0.0.0 unless -p names a host address
LOCAL         REMOTE              PROTO  STATUS
0.0.0.0:8080  192.168.127.2:8080  tcp    ok
```

A real failure looks like this — here macOS's own Control Centre (AirPlay
Receiver) was already holding port 5000:

```text
0.0.0.0:5000  192.168.127.2:5000  tcp    error: another process on this Mac already
holds this host port (lsof -nP -iTCP:5000); publish the container on a different host port
```

> **`-p 8080:80` binds every interface**, as `docker run -p` does on Linux:
> `127.0.0.1`, `::1`, `localhost` **and your LAN address**. Anyone on your
> network can reach that container. `jm set --publish-addr 127.0.0.1` changes
> the default for the machine; `-p 127.0.0.1:8080:80` confines one mapping.

Tidy up:

```bash
jpodman rm -f web
```

---

## Step 6 — look inside the guest with `jm ssh`

`jm ssh` gives you a root shell in the VM, or runs one command there.

```bash
jm ssh -- uname -a
jm ssh -- 'freebsd-version; zpool list; sysctl compat.linux.osrelease'
jm ssh -- podman --version
```

```text
FreeBSD jailmachine 15.1-RELEASE-p2 FreeBSD 15.1-RELEASE-p2 releng/15.1-n283596-aadd58dddcbc GENERIC arm64
15.1-RELEASE-p2
NAME    SIZE  ALLOC   FREE  CKPOINT  EXPANDSZ   FRAG    CAP  DEDUP    HEALTH  ALTROOT
zroot  62.5G  4.11G  58.4G        -         -     1%     6%  1.00x    ONLINE  -
compat.linux.osrelease: 5.15.0
podman version 5.8.4
```

Three facts in that output:

- The guest is a real FreeBSD 15.1 release, on **ZFS**.
- `compat.linux.osrelease: 5.15.0` is the Linux release the Linuxulator claims
  to be. That number is what an Alpine container sees in `uname`.
- The engine in the guest is podman **5.8.4**, while your host client is
  whatever Homebrew gave you (6.1.0 here). That is fine — they speak the same
  API.

`jm ssh` with no command opens an interactive shell. `jm console` shows the
guest's serial console instead, which is where the kernel logs Linuxulator
gaps:

```bash
jm console -n 5
```

```text
linux: jid 23 pid 8547 (setpriv): unsupported prctl option 27
linux: jid 23 pid 8455 (redis-server): syscall membarrier not implemented
linux: jid 43 pid 10221 (caddy): unsupported prctl option 1398164801
```

Those lines are informational — all three of those containers were running
happily.

---

## Step 7 — `jm doctor`

One command checks every host tool, the state root and every machine, and
prints a fix under anything that is not `[ ok ]`.

```bash
jm doctor
```

```text
jm 0.1.2 (8a20dda, 2026-08-21T18:12:58Z)

STATUS  CHECK                        DETAIL
[ ok ]  host                         darwin/arm64
[ ok ]  host resolver                queries go through the host resolver (getaddrinfo)
[ ok ]  backend qemu                 preflight passed
[ ok ]  qemu-system-aarch64 version  11.1.0 at /opt/homebrew/bin/qemu-system-aarch64
[ ok ]  qemu-system-aarch64 hvf      Hypervisor.framework accelerator available
[ ok ]  edk2 firmware                /opt/homebrew/share/qemu
[ ok ]  gvproxy                      0.8.9 at /opt/homebrew/opt/podman/libexec/podman/gvproxy
[ ok ]  podman version               6.1.0 at /opt/homebrew/bin/podman
[ ok ]  ssh                          /usr/bin/ssh
[ ok ]  ssh-keygen                   /usr/bin/ssh-keygen
[ ok ]  xz                           /opt/homebrew/bin/xz
[ ok ]  state root                   /Users/you/.jailmachine
[ ok ]  socket paths                 58 of 103 bytes used
[ ok ]  machine jailmachine          running (qemu, gvproxy)
[ ok ]  resolver jailmachine         127.0.0.1:49900 answers through the host resolver: broadcasthost resolves to [255.255.255.255], as it does on the host
[ ok ]  guest resolver jailmachine   the guest resolves host.containers.internal to 192.168.127.254, this machine's host resolver
[ ok ]  shares jailmachine           4 share(s) at their host path: /Users/you (rw), /Volumes (rw), /private/tmp (rw), /var/folders/5l/x_34hg657n93c50c02sbjb040000gn (rw)
[ ok ]  share parity jailmachine     a file written in /Users/you is in the guest at the same path
[ ok ]  datagram limit jailmachine   published udp carries payloads up to 8972 bytes (gvproxy MTU 9000); larger datagrams are dropped, not fragmented. $JM_MTU changes the link size (576..16384; JM_MTU=1500 matches Docker)
[ ok ]  jpodman                      /opt/homebrew/bin/jpodman
[ ok ]  jdocker                      /opt/homebrew/bin/jdocker
[ ok ]  autostart                    on: jpodman/jdocker boot a stopped machine
[ ok ]  clock jailmachine            in step with the host (jm_rtcsync running)

23 ok, 0 warning(s), 0 failure(s)
```

Twenty-three checks on a machine that exists; the `machine …` rows only
appear once you have created one. Two of them do real work rather than
reading a version string:

- **`share parity`** writes a file on the Mac and asserts a container sees it
  at the same absolute path.
- **`guest resolver`** asks the running guest to resolve a name, over the
  wire, and compares the answer with the host's.

`jm doctor` exits `1` if any check failed; warnings alone still exit `0`.
`--json` prints the same report as one object.

> A long `--state-root` produces a `[warn] socket paths` row: unix socket
> paths are capped at 103 bytes, and jm falls back to `$TMPDIR`. It is a
> warning, not a failure, and the default state root is nowhere near the cap.

---

## Step 8 — `jm stop`

```bash
jm stop
```

```text
==> backend: asking the guest to power off, then stopping qemu
==> network: stopping gvproxy networking
==> done: jailmachine stopped
```

That is a clean guest shutdown, then the hypervisor, then the network
provider, then the forwarder and resolver. It took **13 s** here. `jm stop
--force` skips the guest shutdown, and `jm stop` on a stopped machine is a
no-op — it also repairs a machine left `broken` by a crash or a reboot.

---

## Cleanup

Stopping is enough to reclaim the RAM and the CPU. To get the disk back as
well, delete the machine:

```bash
jm rm                       # stops it, forgets both podman connections, deletes the directory
jm list                     # nothing left
```

To uninstall entirely, remove the binary too:

```bash
brew uninstall --cask jailmachine      # also removes the jpodman/jdocker symlinks
# or: rm -f "$(go env GOPATH)"/bin/{jm,jpodman,jdocker}
rm -rf ~/.jailmachine                  # only needed if a machine was removed by hand
```

Neither `qemu` nor `podman` is removed for you.

---

## Where next

| You want | Go to |
|---|---|
| Edit code on the Mac, run it in a container | [2. Develop on a shared folder](02-develop-on-a-shared-folder.md) |
| More than one container at a time | [3. A stack with compose and kube](03-a-stack-with-compose-and-kube.md) |
| Native FreeBSD images, and jails | [4. FreeBSD images and jails](04-freebsd-images-and-jails.md) |
| Every flag and environment variable | [docs/USAGE.md](../USAGE.md) |
| Something is broken | `jm doctor`, then [docs/TROUBLESHOOTING.md](../TROUBLESHOOTING.md) |

---

*Output on this page was captured on 2026-08-21 against a machine built from
commit `8a20dda` — macOS 26.5 on Apple Silicon, guest FreeBSD 15.1-RELEASE-p2
arm64, guest podman 5.8.4, host podman 6.1.0. Paths and the machine name have
been shortened to the defaults; the text of every command and every message is
verbatim.*
