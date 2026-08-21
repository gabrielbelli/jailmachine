# Using jailmachine

> **Still an MVP — a working demo**, but a wider one than v0.1.0. Everything
> documented on this page is implemented and verified against the binary,
> including host directory mounts at identical paths, name resolution 1:1
> with the host, autostart on demand, the `jdocker` wrapper and
> docker-identical `-p` semantics — a host address in the flag binds that
> address on the **Mac**, as it does under Docker Desktop. UDP works from
> Linux containers as well, publishing included; the one narrow Linuxulator
> gap left is called out where it bites.

Install first: [INSTALL.md](INSTALL.md). When something misbehaves, see
[TROUBLESHOOTING.md](TROUBLESHOOTING.md); for how the pieces fit together,
[ARCHITECTURE.md](ARCHITECTURE.md).

## The shape of it

```bash
jm init      # create a machine: ssh key, verified guest image, first-boot seed
jm start     # boot it, provision it, mount the shares, register podman connections, publish ports
jpodman run --rm --os=linux docker.io/alpine echo hi
```

- One machine is one FreeBSD VM under `~/.jailmachine/machines/<name>/`.
- The guest is FreeBSD, so podman pulls **FreeBSD image variants by
  default**. Linux images run through the Linuxulator and need `--os=linux`
  (or `podman pull --os=linux`).
- Host and guest talk over SSH only; there is no vsock driver, so the engine
  socket is tunnelled over SSH.
- Host directories **are** shared, at the same absolute path in the guest as
  on the Mac, over 9p (the guest has no virtiofs driver). See
  [Sharing host directories](#sharing-host-directories).
- Names resolve exactly as they do on the Mac, because the Mac's own resolver
  answers for the guest. See [Name resolution](#name-resolution).

## Conventions shared by every command

| | |
|---|---|
| `[name]` | Optional machine name. Defaults to `jailmachine`; if that does not exist and exactly one machine does, that one is used (jm says so). With several machines and no `jailmachine`, you must name one — that is a usage error |
| Exit codes | `0` success, `1` failure, `2` usage error (bad flag, bad name, ambiguous default). `jm ssh`, `jm podman` and `jm docker` replace themselves with `ssh`/`podman`/`docker`, so their exit code is that program's |
| Errors | Printed as `jm: <command> <name>: <stage>: <cause>` with a second `hint:` line naming the log to read or the command to run |
| Idempotence | `init`, `start`, `stop` and `rm` converge: re-running a finished step is a no-op, an interrupted one resumes |

### Global flags

| Flag | Effect |
|---|---|
| `--state-root <dir>` | Directory holding all machine state. Default `~/.jailmachine`, or `$JM_HOME` |
| `--json` | Machine-readable output on stdout; progress lines move to stderr. Supported by `list`, `inspect`, `ports`, `doctor` and `version` |
| `-q`, `--quiet` | Suppress `==>` stage lines and progress. Errors still go to stderr |
| `-h`, `--help` | Help for the command |
| `-v`, `--version` | Print the version and exit (root command only) |

---

# Command reference

## `jm init [name]`

Create a machine: generate a dedicated ed25519 SSH key, download and verify
the guest image, grow `disk.raw` to `--disk`, and write the first-boot
NoCloud seed. Nothing boots. Safe to re-run after an interruption —
finished steps are skipped and a partial download resumes.

`init` never falls back to "the only machine": with no name it creates
`jailmachine`. An existing name is refused before anything is touched.

| Flag | Default | Meaning |
|---|---|---|
| `--cpus <n>` | `4` | Virtual CPUs (at least 1) |
| `--memory <MiB>` | `4096` | Memory in MiB (at least 256; a bare number, no units here) |
| `--disk <GiB>` | `64` | Disk size in GiB. `disk.raw` is sparse, so this is a ceiling, not an allocation |
| `--image <ref>` | `prebaked` | Image source — see [Image sources](#image-sources) |
| `--ssh-port <port>` | `2222` | Host loopback port forwarded to the guest's sshd |
| `--mount <dir>[:ro]` | — | Share a host directory with the guest **at the same absolute path**, on top of the defaults. Repeatable; `:ro` makes it read-only. See [Sharing host directories](#sharing-host-directories) |
| `--no-mounts` | — | Share nothing at all, not even the defaults |
| `--publish-addr <addr>` | `0.0.0.0` | **Default** host address published container ports bind to. `127.0.0.1` keeps them off the LAN; a `-p` that names an address of its own binds that one instead. See [Publishing ports](#publishing-ports-and---publish-addr) |

```bash
jm init
jm init --cpus 2 --memory 2048 dev
jm init --image official:15.1-RELEASE --disk 32
jm init --mount /work --mount "/srv/data:ro"       # quote :ro in zsh
jm init --no-mounts --publish-addr 127.0.0.1
```

Exit codes: `0` created; `2` for an invalid flag value or a bad machine
name; `1` for anything else (image download or checksum failure, the name
already exists, a missing host tool).

### Image sources

| `--image` | What you get | Verification | `image_trusted` |
|---|---|---|---|
| `prebaked` (default) | An already-provisioned guest published on this repo's `guest-<version>` GitHub release, the version the binary names. First boot is a boot, nothing more (about 22 s cold) | mandatory `.sha256` sidecar next to the asset, verified by `jm init` | `true` |
| `prebaked:<guest version>` | The same, pinned (e.g. `prebaked:15.1.0`) | as above | `true` |
| `official` | The stock FreeBSD `BASIC-CLOUDINIT-zfs.raw.xz` cloud image from download.freebsd.org, provisioned on first boot (packages installed inside the guest, about 2 minutes) | mandatory `CHECKSUM.SHA256` from the release directory | `true` |
| `official:<release>` | The same, pinned (e.g. `official:15.1-RELEASE`) | as above | `true` |
| a path or an `http(s)` URL to a `.raw` (or `.img`), `.raw.xz` or `.raw.zst` | Bring your own disk, satisfying the guest contract. jm applies the seed; provisioning is yours | a sibling `<file>.sha256` if one exists, otherwise **none** | `false` without a sidecar |

A BYO image without a sidecar prints a warning at `init` and shows
`image_trusted=false` in `jm inspect` forever after. The full contract is in
[guest-contract.md](guest-contract.md).

```bash
jm init --image ~/Downloads/custom.raw.zst      # verified if custom.raw.zst.sha256 sits next to it
jm init --image https://example.invalid/my.raw.xz
```

Plain `http://` URLs are accepted too, but there is no transport security
and, without a `.sha256` sidecar, no integrity check either — prefer
`https`.

## `jm start [name]`

Boot a machine and make it usable, in stages: **network** (start the network
provider) → **dns** (start the host resolver) → **backend** (boot the
hypervisor, with one 9p device per share) → **ssh** (wait for sshd) →
**provision** (wait for the guest's ready marker) → **dns** again (point the
guest at the host resolver and push the search domains) → **connect**
(register the podman connections) → **forwarder** (start the detached
port-publishing loop).

Before the backend stage, `jm start` reconciles the machine's share set
against the host: a shared directory that has vanished (an unplugged disk) is
dropped with one warning rather than refusing to boot, and reappears at the
next start. `$JM_PUBLISH_ADDR`, if set, is folded into the record here so
that what the detached forwarder binds is what `jm inspect` and `jm ports`
show.

Starting a machine that is already running re-checks the ssh, provision,
dns, connect and forwarder stages, so an interrupted start finishes rather
than needing a stop first. A *broken* machine (half of it running, or a stale
pid file) is stopped and started again.

| Flag | Effect |
|---|---|
| `--set-default` | Also make this machine the **default** podman connection. Without it, jm registers the connections but never repoints a default you already had (see [Podman connections](#podman-connections)) |

```bash
jm start
jm -q start && jpodman run --rm --os=linux docker.io/alpine echo hi
```

On failure the error names the stage and the log to read: `qemu.log` and
`console.log` (hypervisor), `gvproxy.log` and `forward.log` (networking),
`forwarder.log` (port publishing), `resolver.log` (name resolution), or
`/var/log/jm-provision.log` inside the guest.

A share the guest cannot mount, or a resolver that will not come up, is a
**warning**, not a failure: the machine still starts. `jm doctor` is what
reports the loss.

Exit codes: `0` running and connected; `1` if any stage failed; `2` for a
usage error.

## `jm stop [name]`

Stop the port forwarder first (so it can unexpose the mappings it owns
while the network provider is still up), ask the guest to power off over
SSH, then drive the hypervisor down (QMP `system_powerdown`, then SIGTERM
and SIGKILL), then stop the network provider. Stopping a stopped machine is
a no-op. It also repairs a "broken" machine (a pid file with no process).

| Flag | Effect |
|---|---|
| `-f`, `--force` | Terminate the hypervisor without asking the guest to shut down |

```bash
jm stop
jm stop --force dev
```

Exit codes: `0` stopped (including "already stopped"); `1` if a component
refused to stop; `2` usage.

## `jm ssh [name] [-- command...]`

Open an interactive root shell in the guest, or run one command. If the first
argument is not an existing machine name, every argument is taken as the
command and the default machine is used — the `--` is optional but makes the
intent obvious.

No flags of its own: flags after the machine name belong to the remote
command.

```bash
jm ssh
jm ssh -- uname -srm            # FreeBSD 15.1-RELEASE-p2 arm64
jm ssh dev tail -f /var/log/jm-provision.log
```

`jm ssh` replaces itself with `ssh`, so **the exit code is the remote
command's** (or `255` when the connection itself fails). It refuses with
exit `1` if the machine is not running.

## `jm list` (alias `jm ls`)

List every machine with its runtime state. Nothing is cached: the
hypervisor and the network provider are asked on every call.

Columns: `NAME STATE CPUS MEMORY DISK SSH PORTS` — `PORTS` is the number of
published container ports.

```bash
jm list
```

```
NAME         STATE    CPUS  MEMORY    DISK    SSH             PORTS
jailmachine  running  4     4096 MiB  64 GiB  127.0.0.1:2222  0
```

`--json` prints the same records as `jm inspect --json` in an array:

```bash
jm ls --json | jq -r '.[] | select(.state == "running") | .name'
```

Exit code `0` even when there are no machines (an empty table).

## `jm inspect [name]`

Show one machine's record plus its computed runtime state.

```bash
jm inspect
```

```
Name:           jailmachine
State:          running
Backend:        qemu
Network:        gvproxy
Image:          prebaked:15.1.0
Image trusted:  true
CPUs:           4
Memory:         4096 MiB
Disk:           64 GiB
MAC:            5a:94:ef:e4:0c:ee
Guest IP:       192.168.127.2
DNS:            192.168.127.2
SSH:            root@127.0.0.1:2222
SSH key:        /Users/you/.jailmachine/machines/jailmachine/ssh/id_ed25519
Podman:         ssh://root@127.0.0.1:2222/var/run/podman/podman.sock (jailmachine)
Podman socket:  unix:///Users/you/.jailmachine/machines/jailmachine/podman.sock (jailmachine-sock)
Docker host:    unix:///Users/you/.jailmachine/machines/jailmachine/podman.sock (jdocker)
Autostart:      on
Console:        /Users/you/.jailmachine/machines/jailmachine/console.log
Network log:    /Users/you/.jailmachine/machines/jailmachine/gvproxy.log
Network log:    /Users/you/.jailmachine/machines/jailmachine/forward.log
Resolver:       running
Resolver address: 127.0.0.1:53042
Resolver log:   /Users/you/.jailmachine/machines/jailmachine/resolver.log
Publish address: 0.0.0.0
Forwarder:      running
Forwarder log:  /Users/you/.jailmachine/machines/jailmachine/forwarder.log
Port:           0.0.0.0:8080 -> 192.168.127.2:8080 tcp (ok)
Share:          /Users/you (rw)
Share:          /Volumes (rw)
Share:          /private/tmp (rw)
Share:          /var/folders/qb/8x1s7c9d0000gn (rw)
Dir:            /Users/you/.jailmachine/machines/jailmachine
Provisioned:    true
Created:        2026-08-21T02:37:54Z
```

One `Port:` line is printed per published mapping and one `Share:` line per
shared host directory, between `Forwarder log:` and `Dir:`; a machine
publishing nothing, or sharing nothing, has none. A share is annotated
`— missing on the host, not shared` when its path has vanished, and
`— ignored: backend "<name>" cannot share host directories` when the backend
has no file-sharing capability at all.

`--json` prints one object with snake_case keys: `name`, `state`
(`running` | `stopped` | `broken`), `backend_state`, `network_state`,
`backend`, `network`, `image`, `cpus`, `memory_mib`, `disk_gib`, `mac`,
`ssh_port`, `ssh_user`, `guest_ip`, `ssh` (host:port), `ssh_key`,
`podman_uri`, `podman_sock_uri`, `docker_host`, `api_socket`, `dns`,
`console`, `network_logs`, `dir`, `provisioned`, `image_trusted`, `created`,
`version`, `backend_opts`, `ports` (array of `{proto, local, remote, since,
error}`), `forwarder_state`, `forwarder_log`, `publish_addr_effective`
(what the running forwarder binds when `-p` names no host address),
`publish_addr_pending` (the record's value when it differs and is waiting for
a restart),
`shares` (array of `{host_path, guest_path, read_only, tag}`),
`file_sharing`, `resolver_state`, `resolver_addr`, `resolver_log` and
`autostart`. Empty values are omitted.

```bash
jm inspect --json | jq -r .podman_sock_uri
```

Exit codes: `0`; `1` if the machine does not exist or its state cannot be
read; `2` usage.

## `jm rm [name]`

Stop the machine if needed, remove both podman connections, drop the guest's
`known_hosts` entry, and delete the machine directory. A **named** machine
always converges to "gone": removing one that does not exist prints
`<name> does not exist; nothing to remove` and exits `0`. The one exception
is `jm rm` with no argument on a host with no machines at all — there is
nothing to resolve the default against, so it fails with
`jm: rm: no machines exist` and exit `1`.

Unlike other commands, `rm` looks at the directory rather than the record,
so a half-initialised machine whose `machine.json` is missing or corrupt can
still be cleaned up. A name given on the command line is always taken
literally.

| Flag | Effect |
|---|---|
| `-f`, `--force` | Kill the hypervisor and ignore errors along the way |

```bash
jm rm
jm rm --force dev
```

Exit codes: `0` gone; `1` if the machine could not be stopped (the hint
suggests `--force`); `2` usage.

## `jm env [name]`

Print the shell exports a podman or docker client needs to reach the
machine's engine through the network provider's host-side unix socket.

| Flag | Default | Effect |
|---|---|---|
| `--shell <sh\|fish>` | `sh` | Output syntax. Anything else is a usage error |

```bash
eval "$(jm env)"
eval (jm env dev --shell fish)
```

```
export CONTAINER_HOST="unix:///Users/you/.jailmachine/machines/jailmachine/podman.sock"
export CONTAINER_CONNECTION="jailmachine-sock"
export DOCKER_HOST="unix:///Users/you/.jailmachine/machines/jailmachine/podman.sock"
# run: eval "$(jm env)"
```

`CONTAINER_CONNECTION` names the **socket** connection, because podman
resolves it before `CONTAINER_HOST`.

The exports are only useful while the machine is **running**: `jm env` on a
stopped machine still exits `0` and still prints them, but the socket they
name does not exist, so every podman and docker call in that shell fails
with a connection error until you `jm start`.

Requires a provider that proxies the API socket — that is gvproxy, the
default. With `JM_NETWORK=user` there is no socket and `jm env` fails with
exit `1`, pointing at the `ssh://` connection instead.

## `jm ports [name]`

List the host to guest port mappings the forwarder owns, with the outcome of
the last attempt. It reads the machine's `forwards.json` and never blocks on
the machine.

```bash
jm ports
```

```
# publishing on 0.0.0.0 unless -p names a host address
LOCAL           REMOTE              PROTO  STATUS
0.0.0.0:8080    192.168.127.2:8080  tcp    ok
0.0.0.0:5432    192.168.127.2:5432  tcp    error: another process on this Mac already holds this host port
127.0.0.1:8082  192.168.127.2:8082  tcp    ok
[::1]:8087      192.168.127.2:8087  tcp    ok
```

`LOCAL` is the host side, `REMOTE` the guest side. Rows no longer share one
host address: `-p 8080:80` binds the machine's **publish address** (`0.0.0.0`
by default — every interface, as `docker run -p` does on Linux), while
`-p 127.0.0.1:8082:80` binds your loopback and only that. The `#` comment
line names the default, not the whole table.

After `jm set --publish-addr` on a running machine, a second `#` line names
the record's new address and says the running forwarder keeps the old one
until `jm stop && jm start`. `jm inspect` says the same on its
`Publish address:` row.

`STATUS` is `ok`, or `error: <cause>` — a host port already in use, an
address your Mac does not have, a provider that could not be reached, or a
guest-side redirect not yet in place. Failed mappings are retried on every
resync, so the error is a live status, not a permanent verdict.

If the forwarder is not running, a `#` comment line says so above the table.
The exit code is still `0`.

`--json` prints the entries as an array of `{proto, local, remote, since,
error}` (an empty array when there are none).

## `jm set [name]`

Change a machine's resources.

| Flag | Requires | Meaning |
|---|---|---|
| `--cpus <n>` | machine stopped | Virtual CPUs, 1–256 |
| `--memory <size>` | machine stopped | Memory: a bare number is MiB, or use a unit — `4096MiB`, `4GiB`, `4g`. Between 256 MiB and 1 TiB |
| `--disk <GiB>` | stopped **or** running | Disk size, **grow only**, 1–16384 GiB |
| `--ssh-port <port>` | machine stopped | Host port forwarded to the guest's sshd, 1–65535 |
| `--mount <dir>[:ro]` | takes effect on the next start | Share a host directory at the same absolute path. Repeatable |
| `--unmount <dir>` | takes effect on the next start | Stop sharing a host directory. Repeatable |
| `--publish-addr <addr>` | takes effect on the next start | **Default** host address published ports bind to; a `-p` that names one binds that instead |

`--mount`, `--unmount` and `--publish-addr` are accepted while the machine
runs — they are recorded, and jm prints the `jm stop && jm start` needed to
apply them.

`--disk` extends `disk.raw` sparsely. On a running machine the guest's
partition and ZFS pool are extended immediately; on a stopped one they are
extended on the next `jm start`. If the guest side fails, the record keeps a
pending flag and the next start retries it.

```bash
jm stop && jm set --cpus 8 --memory 8GiB && jm start
jm set --disk 128                       # works while running
jm set --mount /work --unmount /Volumes # recorded now, applied at the next start
jm set --mount "${P}:ro"                # quote the :ro suffix in zsh
jm set --publish-addr 127.0.0.1
```

Exit codes: `0`; `2` for no flags at all, an out-of-range value or a
shrink; `1` when the machine is running but the change needs it stopped, or
when a grow failed part-way.

## `jm console [name]`

Print the tail of the guest's serial console log, as written by the
hypervisor. This is the only way to watch a boot that never reaches sshd.

| Flag | Default | Effect |
|---|---|---|
| `-n`, `--lines <n>` | `50` | Trailing lines to print |
| `-f`, `--follow` | off | Keep printing new output until Ctrl-C. With `-f` a log that does not exist yet is waited for |

```bash
jm console -n 200
jm console -f            # follow a boot
```

Exit codes: `0`; `1` if there is no console log yet and `-f` was not given,
or the backend has no serial console; `2` usage.

## `jm doctor`

Check the host for everything jm needs: the QEMU backend preflight,
`qemu-system-aarch64` and its version (8+), the HVF accelerator, the EDK2
firmware, `gvproxy`, `podman` and its version (5+), `ssh`, `ssh-keygen`,
`xz` (optional — a warning, not a failure), the state root, the length of
the socket paths it implies, and one row per machine directory.

Each row that is not `[ ok ]` carries a `fix:` line. See
[INSTALL.md](INSTALL.md#verify-the-install) for a full sample report.

```bash
jm doctor
jm doctor --json | jq -r '.checks[] | select(.status != "ok")'
```

`jm doctor` inspects the default state root unless `--state-root` is given.

Exit codes: `0` when nothing failed (warnings are fine); `1` when at least
one check failed.

## `jm image build`

**Maintainers only.** Build the prebaked guest image that `jm init` fetches
by default. Users never need it.

It provisions a throwaway machine `jm-image-build` from the official FreeBSD
image under `<out>/.work` (using host SSH port `2229`), seals it over SSH
with `guest/seal.sh` (drops keys, host keys, logs and the pkg cache,
restores `/firstboot`, trims free space), powers it off, compresses
`disk.raw` with `zstd -19` and writes a `.sha256` sidecar. The 15.1 build
shipped as `guest-15.1.0` is a sealed **802 MiB** `.zst` (841,246,026
bytes); `jm init` checks that sidecar before it writes anything.

| Flag | Default | Meaning |
|---|---|---|
| `--release <rel>` | `15.1-RELEASE` | FreeBSD release to build from (official image) |
| `--out <dir>` | `dist` | Output directory; the build machine lives in `<out>/.work` |
| `--keep` | off | Keep `<out>/.work` (the sealed `disk.raw` and logs) after the build |

```bash
jm image build --release 15.1-RELEASE --out dist
make image                       # the same thing
```

Output: `<out>/jailmachine-guest-<guest version>-freebsd<release>-arm64-zfs.raw.zst`
plus its `.sha256`, to be published as assets of the GitHub release tagged
`guest-<guest version>`. Requires `zstd` on `PATH`. The build registers a
podman connection named `jm-image-build` while it runs and removes it again
with `jm rm` at the end; a default connection you already had is left alone.

`jm image` on its own prints help.

Exit codes: `0`; `1` on any failure of the underlying init/start/seal/stop.

## `jm podman [--no-autostart] [podman args...]` (and `jpodman`)

Run the host `podman` client against a machine without touching your default
podman connection: jm execs `podman --connection <machine> <your args>`.

Machine selection: `$JM_MACHINE` if set, otherwise the usual default
resolution. **Every argument is passed through untouched** — jm parses none
of podman's flags — so podman's own global flags work wherever podman
accepts them:

```bash
jm podman run --rm --os=linux docker.io/alpine echo hi
jpodman build -t myapp .
jpodman --log-level debug ps      # podman's global flag, before the subcommand
jpodman ps --help                 # podman's own help for ps
jpodman --version                 # podman's version
JM_MACHINE=dev jpodman ps
```

jm's own global flags are parsed only when they come **before** the
subcommand of `jm` itself:

```bash
jm --state-root /tmp/jm-test podman ps
```

**Autostart.** A machine that is not running is started first, with one line
on stderr while it boots. Invocations the client answers on its own
(`--help`, `-h`, `help`, `--version`, `-v`, `-V`, `completion`) never boot
anything. To fail instead of booting:

```bash
jpodman --no-autostart ps         # only as the first argument
JM_AUTOSTART=0 jpodman ps
```

Note that `podman version` (the subcommand, not the flag) *does* start the
machine: it reports the engine's version, which is a fact about the machine.

With several machines and none named `jailmachine`, `jpodman` cannot pick
one; the error suggests `JM_MACHINE=<name> jpodman ...`, because this command
takes no machine-name argument.

**`jpodman compose` is the one argument jm looks at.** `podman compose` is a
shim: podman hands the work to an external provider on the host (Docker
Compose, or `podman-compose`) and passes it the connection's URI. Its
`ssh://` URI would send the provider looking for a docker daemon inside the
guest, which fails with `docker system dial-stdio … exit status 255`, so a
`compose` invocation is pointed at the machine's socket connection
(`<name>-sock`) with `DOCKER_HOST` and `CONTAINER_HOST` set to that unix
socket. Every other invocation keeps the SSH connection, untouched.

```bash
jpodman compose up -d
jpodman kube play pod.yaml     # the FreeBSD-native route, no provider needed
```

Note that the podman wrapper sets **no** default platform, unlike `jdocker`:
a Linux image needs `platform: linux/arm64` in the service, or a pre-pull
with `--os=linux`. See
[Compose and Kubernetes YAML](#compose-and-kubernetes-yaml).

`jpodman` is a symlink to `jm` — invoked under that name, `jpodman X` runs
`jm podman X`. The exit code is podman's own.

## `jm docker [--no-autostart] [docker args...]` (and `jdocker`)

Run the host `docker` CLI (and `docker compose`) against a machine's engine,
leaving your docker contexts alone: jm execs `docker` with `DOCKER_HOST`
pointing at the machine's socket and `DOCKER_CONTEXT` dropped from the
environment. podman's API socket serves the Docker API, so the docker CLI,
compose and anything else speaking `DOCKER_HOST` work unchanged.

Needs the docker CLI on the host (`brew install docker` — the client only;
jm is the engine) and a network provider that proxies the engine API onto a
host socket (gvproxy, the default).

```bash
jdocker run --rm docker.io/alpine echo hi
jdocker compose up -d
JM_MACHINE=dev jdocker ps
```

**Platform.** The docker CLI has no `--os` flag — it rejects one outright —
and the guest engine is FreeBSD, so a bare `docker pull alpine` would ask
the registry for OS `freebsd` and fail. The wrapper therefore defaults
`DOCKER_DEFAULT_PLATFORM=linux/arm64`, and a plain `jdocker run alpine`
pulls the Linux image as it would on Docker Desktop. A value you set
yourself wins, as does an explicit `--platform`:

```bash
# opt out (an empty value counts as "chosen"): the engine's own OS is used
DOCKER_DEFAULT_PLATFORM= jdocker run --rm ghcr.io/freebsd/freebsd-runtime:15.1 uname -srm
export DOCKER_DEFAULT_PLATFORM=freebsd/arm64   # or pin it for the shell
```

**Compose.** Because `DOCKER_HOST` is all the Compose plugin needs,
`jdocker compose` drives the guest's podman with nothing else to configure —
including host bind mounts, which resolve because shares sit at identical
paths. The default platform above applies to compose too, so most Linux
services come up without a `platform:` line; the full story, and the two
other routes, are in
[Compose and Kubernetes YAML](#compose-and-kubernetes-yaml).

Machine selection, argument pass-through, autostart and `--no-autostart`
behave exactly as for `jm podman`. `jdocker` is a symlink to `jm`; the exit
code is docker's own.

## `jm version`

Print the build identity: version, commit, build date, Go version and
os/arch.

```bash
jm version
```

```
jm version dev
  commit:     none
  built:      unknown
  go version: go1.26.5
  os/arch:    darwin/arm64
```

`dev`/`none`/`unknown` means an unstamped build (`go build`, `go install`);
a release or `make build` binary carries its tag and commit. `--json`
prints `{version, commit, date, go_version, os, arch}`. `jm --version`
prints just the version string.

## `jm completion <shell>`

Cobra's generated shell completion script (`bash`, `zsh`, `fish`,
`powershell`). `jm completion --help` explains how to install it for your
shell.

## `jm _forwarder`, `jm _resolver` (internal)

The hidden foreground entry points of the port-publishing loop and the host
name resolver. `jm start` launches each detached and `jm stop` terminates
them; there is no reason to run either by hand. They are documented here only
so you recognise them in `ps` output and in `forwarder.log` / `resolver.log`.

---

# Environment variables

Every one of these is read by the binary; there is no `JM_SSH_PORT` — the
SSH port is a flag (`jm init --ssh-port`, `jm set --ssh-port`) stored in the
machine record.

| Variable | Read by | Effect |
|---|---|---|
| `JM_HOME` | all commands | State root, same as `--state-root`. The flag wins if both are given. Default `~/.jailmachine` |
| `JM_MACHINE` | `jm podman` / `jpodman`, `jm docker` / `jdocker` | Which machine to talk to, instead of the default resolution |
| `JM_AUTOSTART` | the client wrappers | `JM_AUTOSTART=0` (also `false`, `no`, `off`) makes `jpodman`/`jdocker` fail on a stopped machine instead of starting it |
| `JM_NO_AUTOSTART` | the client wrappers | The same switch spelt the other way round: `JM_NO_AUTOSTART=1` disables autostart |
| `JM_PUBLISH_ADDR` | `jm start` | Host address published container ports bind to, folded into the machine record at start so `jm inspect` and `jm ports` show what the detached forwarder really binds. Same values as `--publish-addr` |
| `DOCKER_DEFAULT_PLATFORM` | `jm docker` / `jdocker` | Defaulted to `linux/<arch>` by the wrapper; set it yourself (even to the empty string) to opt out and pull the engine's own OS |
| `JM_NETWORK` | `jm init` (recorded), providers | Network provider to create a machine with: `gvproxy` (default) or `user` (QEMU slirp: no `jm env`, no port publishing). Fixed at init — a machine keeps the networking it was created with |
| `JM_BACKEND` | `jm init` (recorded), backends | Force a backend name. Needed on Linux, where there is no default backend yet (`JM_BACKEND=qemu`) |
| `JM_QEMU_ACCEL` | QEMU backend | Accelerator override. Default `hvf` on macOS, `kvm` on Linux; `JM_QEMU_ACCEL=tcg` is pure emulation with `-cpu cortex-a72`, an order of magnitude slower — for building images on machines without a hypervisor, not for using them. Start-stage timeouts are stretched eightfold under TCG |
| `JM_9P_SECURITY` | `jm start` (QEMU backend) | 9p security model for the share devices: `mapped-xattr` (the default), `none` or `mapped-file`. Anything else falls back to the default. Read from the environment at every `jm start` and **not** stored in the machine record, so a machine uses whatever was set when it was last started. `mapped-xattr` keeps guest ownership and modes in host xattrs, which is what lets a container running as root rewrite its own files; `none` passes the Mac's own modes straight through and breaks that. See [Modes and ownership on a share](#modes-and-ownership-on-a-share) |
| `JM_IMAGE_BASEURL` | prebaked image source | Fetch the prebaked image and its `.sha256` from `$JM_IMAGE_BASEURL/<file name>` instead of the GitHub release. For testing an image that is not published yet |
| `JM_GVPROXY` | gvproxy provider | Path to the `gvproxy` binary, instead of `PATH` and then `/opt/homebrew/opt/podman/libexec/podman/gvproxy` |
| `JM_MTU` | `jm start` (gvproxy provider) | Link size gvproxy and the guest agree on. Default **9000**, the virtio-net jumbo frame; clamped to **576–16384** (a value below 576 or that is not a number falls back to the default, one above 9000 clamps to 9000). It caps published UDP at the MTU less 28 bytes — 8972 by default, 1472 with `JM_MTU=1500`, which is Docker's link size. Read from the environment at every `jm start` and **not** stored in the machine record, so a machine uses whatever was set when it was last started; the guest picks it up over DHCP. See [Datagrams are capped at 8972 bytes](#datagrams-are-capped-at-8972-bytes) |
| `JM_E2E` | `make e2e` | `JM_E2E=1` enables the end-to-end test; it is skipped otherwise |

Testing an unpublished guest image:

```bash
(cd dist && python3 -m http.server 8000) &
JM_IMAGE_BASEURL=http://127.0.0.1:8000 jm --state-root /tmp/jm-test init test
JM_IMAGE_BASEURL=http://127.0.0.1:8000 jm --state-root /tmp/jm-test start test
```

---

# State directory layout

Everything jm creates at runtime lives under the state root
(`~/.jailmachine` by default). Nothing is written anywhere else, so
`rm -rf ~/.jailmachine` is a complete uninstall of state.

```
~/.jailmachine/
└── machines/
    └── <name>/
```

| File | What it is |
|---|---|
| `machine.json` | The machine record: name, backend, network, image, cpus, memory, disk, MAC, ssh port and user, guest IP, created, provisioned, image_trusted |
| `machine.lock` | Per-machine advisory lock. A second jm command touching the same machine is refused with `another jm command is operating on "<name>"; try again shortly` |
| `disk.raw` | The guest disk, sparse, `--disk` GiB in size |
| `disk.raw.untrusted` | Present only for a BYO image installed without a checksum; it keeps `image_trusted=false` sticky across an interrupted init |
| `seed.iso` | The NoCloud first-boot seed (label `cidata`): hostname, SSH public key, provisioning script |
| `efivars.fd` | EDK2 UEFI variable store |
| `ssh/id_ed25519`, `ssh/id_ed25519.pub` | The machine's dedicated SSH key pair |
| `console.log` | Guest serial console, what `jm console` prints |
| `qemu.log` | QEMU's own stdout and stderr |
| `qemu.pid` | Hypervisor pid |
| `qmp.sock` | QMP control socket (ACPI power-off, live disk resize) |
| `gvproxy.log`, `gvproxy.pid` | The network provider |
| `net.sock` | QEMU's stream netdev endpoint into gvproxy |
| `api.sock` | gvproxy's HTTP control API (used to expose and unexpose ports) |
| `podman.sock` | Host side of the guest's podman API socket — what `jm env` points clients at |
| `forward.log`, `forward.pid` | The `ssh -N -L` helper that serves `podman.sock` |
| `forwarder.log`, `forwarder.pid` | The detached port-publishing loop |
| `forwards.json` | The mappings the forwarder owns, with the last error per mapping; what `jm ports` reads |
| `resolver.log`, `resolver.pid` | The detached host resolver that answers the guest's DNS queries |
| `resolver.addr` | The `127.0.0.1:<port>` the resolver listens on, which the guest's `local_unbound` forwards to |
| `guest/shares.tab` | The share table, exported to the guest read-only as the `jmconf` 9p share so it can mount the shares declaratively at boot |

> Long state-root paths can overflow the 103-byte unix socket path limit;
> `jm doctor` has a `socket paths` check for exactly that and suggests a
> shorter `--state-root`.

---

# Podman connections

`jm start` registers **two** podman connections per machine and never
repoints a default connection you already had:

| Connection | URI | Transport |
|---|---|---|
| `<name>` | `ssh://root@127.0.0.1:<ssh-port>/var/run/podman/podman.sock` | SSH, with the machine's key as `--identity` |
| `<name>-sock` | `unix:///Users/you/.jailmachine/machines/<name>/podman.sock` | The host-side socket the SSH helper serves from the guest's podman socket |

**Why jm does not repoint your default:** your `podman` may already point
at a `podman machine`, a remote host, or a colleague's socket. Silently
repointing it would break those. So plain `podman` keeps doing whatever it
did before, and you reach the FreeBSD machine explicitly:

```bash
jpodman ps                     # podman --connection jailmachine ps
podman --connection jailmachine ps
eval "$(jm env)"; podman ps    # via the socket connection
```

Opt in if you want plain `podman` to mean the machine:

```bash
jm start --set-default
```

> **One exception, and it is podman's rather than jm's:** if you had no
> podman connections at all — a fresh Mac — podman promotes the first
> connection it is ever given, so `<name>` becomes your default the first
> time round and plain `podman` reaches the machine. jm never touches a
> default you already had. `podman system connection ls` shows which
> connection is `Default`.

`jm rm` removes both connections again. If a connection is left behind after
a manual cleanup, `podman system connection remove <name>` finishes the job.

## Docker CLI and compose

`jdocker` is the direct route: it points the docker CLI at the machine for
one command, leaving your contexts alone (see
[`jm docker`](#jm-docker---no-autostart-docker-args-and-jdocker)). `jm env`
is the other route — it exports `DOCKER_HOST` (and `CONTAINER_HOST`) pointing
at the same unix socket, which is all the docker CLI and docker-compose need
if you would rather point a whole shell:

```bash
jdocker ps
jdocker compose up -d

eval "$(jm env)"             # or point the shell yourself
docker ps
docker version               # server: freebsd/arm64/freebsd-15.1
docker compose up -d         # see Compose and Kubernetes YAML
```

**Compose has a section of its own.** All three orchestration routes —
`jdocker compose`, `jpodman compose` and `jpodman kube play` — with full
examples, the `platform:` / `--os=linux` rule and the healthcheck caveat, are
in [Compose and Kubernetes YAML](#compose-and-kubernetes-yaml).

This is socket-level compatibility rather than a reimplementation: the docker
CLI talks to podman's Docker-compatible API, and `jdocker` only sets
`DOCKER_HOST` and a default platform for it. Anything the docker CLI can ask
podman for works; anything podman's API does not implement does not.

## Multiple machines

Machines are fully independent — their own disk, key, ports and podman
connections. Give each one its own SSH port.

```bash
jm init --cpus 2 --memory 2048 --ssh-port 2223 dev
jm start dev
jm list

JM_MACHINE=dev jpodman ps          # dev's engine
JM_MACHINE=dev jdocker ps          # the same, through the docker CLI
jm ssh dev -- uname -a
jm stop dev
```

With several machines and no machine literally named `jailmachine`, a
command without a name is a usage error listing the candidates.

---

# Sharing host directories

Host directories are visible inside the guest — and so inside every container
— **at the same absolute path they have on the Mac**. That is the whole rule
(ADR 0007): jm never rewrites a `-v` argument, and there is no `/host_mnt`
prefix to learn.

```bash
jpodman run --rm --os=linux -v ~/code:/app docker.io/alpine ls /app
jpodman run --rm --os=linux -v ~/code:"$HOME/code" docker.io/alpine ls "$HOME/code"
jdocker run --rm -v "$PWD:$PWD" -w "$PWD" docker.io/alpine ls
```

## What is shared by default

A new machine shares four roots, skipping any the host does not have:

| Root | Why |
|---|---|
| Your home directory | Where your work is |
| `/Volumes` | Where macOS mounts removable and network volumes |
| `/private/tmp` | The real location of `/tmp` |
| `$TMPDIR`'s parent (`/var/folders/<hash>`) | Where `mktemp -d`, `os.MkdirTemp` and every test harness put scratch directories |

`jm inspect` lists the set with one `Share:` line each, and `jm doctor`
checks parity for real: it writes a file on the host and asserts a container
sees it at the same path.

> **Every container can read and write everything shared.** By default that
> includes your whole home directory — `~/.ssh`, `~/.aws`, browser profiles,
> and `~/.jailmachine` itself, which holds each machine's private SSH key. A
> container you `run` can therefore read those keys and write to that state
> root. This is the same posture as Docker Desktop's default and it is a
> deliberate one, but if you run images you do not trust, narrow it:
>
> ```bash
> jm set --no-mounts --mount ~/code --mount "/srv/data:ro"
> ```

## `/tmp` is the one path that cannot follow the rule

On macOS `/tmp` is a symlink to `/private/tmp`, and a share mounted at the
guest's own `/tmp` would shadow the guest's own temporary directory. So jm
shares `/private/tmp` and leaves your argument alone:

```bash
jpodman run --rm --os=linux -v /private/tmp/x:/app docker.io/alpine ls /app   # yes
jpodman run --rm --os=linux -v /tmp/x:/app docker.io/alpine ls /app           # empty
```

The second silently binds the **guest's** own empty `/tmp/x` — no error from
jm and none from podman. `$TMPDIR` and `mktemp -d` hand out `/var/folders/...`
paths, which are shared, so those need no thought.

## Changing the set

| Command | Effect |
|---|---|
| `jm init --mount <dir>[:ro]` | Add a root on top of the defaults, at creation |
| `jm init --no-mounts` | Create the machine sharing nothing |
| `jm set --mount <dir>[:ro]` | Add a root to an existing machine |
| `jm set --unmount <dir>` | Remove one |
| `jm set --no-mounts` | Clear the set |

A share is named by its **host path alone** — that is also its guest path —
so the only suffix is `:ro` (or `:rw`, the default). `~` is expanded, paths
are canonicalised, and a path that does not exist yet is kept: an unplugged
disk is dropped at `jm start` with one warning and comes back when it does.

`--mount` and `--unmount` are recorded immediately and applied at the next
`jm stop` + `jm start`; jm prints the commands.

> **zsh: quote the `:ro` suffix.** In zsh, `:ro` at the end of an unquoted
> word is a *history modifier*, so `jm set --mount $P:ro` fails before jm ever
> sees it (`zsh: no such file or directory`, or a bad-modifier error). Quote
> the whole value:
>
> ```bash
> jm set --mount "${P}:ro"
> jpodman run --rm --os=linux -v "${P}:${P}:ro" docker.io/alpine ls "$P"
> ```
>
> bash is unaffected, but quoting is harmless there.

## Modes and ownership on a share

Shares are exported with the 9p **`mapped-xattr`** security model: the guest's
ownership and modes are kept in host extended attributes rather than applied
to the host file. The alternative, `none`, passes the Mac's own modes through
unchanged — which reads better on the Mac, but the host end of the share runs
as your unprivileged Mac user, so a container running as root cannot write a
file it has just made read-only. macOS enforces the mode even for the file's
owner. Git does exactly that with its pack temp files, so under `none` a clone
into a shared directory fails outright:

```text
fatal: Unable to create temporary file '.../.git/objects/pack/tmp_pack_XXXXXX': Permission denied
```

Under `mapped-xattr` the same clone succeeds. What each end sees:

| Created by | Mode | In the container | On the Mac |
|---|---|---|---|
| The Mac | `0755` | `-rwxr-xr-x`, owned by uid `501` — and a **non-root** container user can read it | `-rwxr-xr-x`, exactly as written |
| A container running as root | `0700` | `-rwx------`, owned by uid `0` | `-rw-------`, with the real mode and owner in the `user.virtfs.mode` / `user.virtfs.uid` xattrs |

The cost is therefore cosmetic and host-side: a file a container creates looks
`0600` in `ls -l` on the Mac, and its executable bit lives in an xattr
(`ls -l@`, `xattr -p user.virtfs.mode <file>`). Read-only shares stay
read-only from the guest under either model.

`$JM_9P_SECURITY` picks the model, read at every `jm start` and not stored in
the machine record:

```bash
JM_9P_SECURITY=none jm start          # host-native modes; root in a container loses
JM_9P_SECURITY=mapped-file jm start   # metadata in a sidecar directory instead of xattrs
jm stop && jm start                   # back to the default, mapped-xattr
```

Prefer `none` only if host-side modes matter more to you than containers that
run as root — a `git clone`, `npm install` or any build that chmods its own
temporary files will fail there.

## What 9p does not do

The transport is virtio-9p, because the FreeBSD guest has no virtiofs driver.
Semantics are best-effort POSIX and the gaps are contractual:

| Gap | Effect |
|---|---|
| No `inotify`/`kqueue` events ([#4](https://github.com/gabrielbelli/jailmachine/issues/4)) | A file watcher in a container never fires on a host-side write, though reads are coherent straight away. Use a polling watcher: `CHOKIDAR_USEPOLLING=1` (chokidar, and everything built on it), `nodemon --legacy-watch`, `--watch.usePolling` for Vite and Vitest |
| `utimes` is a silent no-op | Explicitly-set timestamps do not stick; `make` and other mtime-driven tools can misbehave on a shared tree |
| Guest ownership and modes live in host xattrs | Under `mapped-xattr` a `chown` in the guest sticks, but it is recorded in `user.virtfs.uid`/`user.virtfs.gid` rather than applied to the host file, and device and fifo nodes are plain host files carrying their real type in `user.virtfs.mode`. See [Modes and ownership on a share](#modes-and-ownership-on-a-share) |
| Throughput ~70 MB/s, and metadata much slower still — creating 1000 small files takes **3.6 s** on a share against **0.76 s** on the guest's own disk ([#4](https://github.com/gabrielbelli/jailmachine/issues/4)) | Large builds and dependency installs are noticeably slower |

Shares are for source trees and data. **Keep build output, image layers and
databases in an engine-managed volume** (`-v myvol:/out`), which lives on the
guest's ZFS and is the fast, faithful path.

---

# Name resolution

Whatever resolves on your Mac resolves in the guest and in its containers,
with the same answer (ADR 0008). `jm start` runs a small resolver on the host
and gives the guest exactly one nameserver: that resolver. Queries are
answered through **macOS's own resolution API**, the same path a host
application takes, so host policy applies without jm modelling any of it.

| Works inside a container | Because |
|---|---|
| Split-horizon and VPN names | The host's scoped, per-domain and interface-scoped resolvers are consulted |
| `/etc/hosts` entries on the Mac | The host resolver reads them |
| Short names via your search domains | The effective search list comes from `scutil --dns`, is re-read every 30 s and pushed into the guest without a restart |
| `.local` mDNS names | Multicast discovery happens on the host |
| `.test`, `.invalid`, `.home.arpa`, `.onion` | The guest's blackhole zones for these RFC 6761 TLDs are disabled deliberately, so a `.test` name in your hosts file resolves in a container |
| `host.docker.internal`, `host.containers.internal` | Answered locally as the address that means "the host" from inside the guest |
| The Mac's own hostname and `.local` name | Answered locally, and to the host — never to something inside the guest |

```bash
jpodman run --rm --os=linux docker.io/alpine ping -c1 host.docker.internal
jpodman run --rm --os=linux docker.io/alpine nslookup something.internal
jm doctor      # asserts a host-only name resolves in the guest to the right address
```

A host answer of `127.0.0.1` is rewritten to the host alias, so a service
listening on your Mac's loopback is reachable from a container. An answer of
`0.0.0.0` is dropped instead: that is how a hosts file blocks a name, and the
guest cannot reach it either. `AAAA` answers `NODATA`, because the guest
network is IPv4-only.

**Failure is propagated, never papered over.** If the host resolver errors,
the guest fails exactly where the host would; jm will not fall back to a
public resolver, because on a split-horizon network that answers an internal
name with a public address, and a wrong answer is worse than no answer.

If the resolver cannot be brought up at all, `jm start` warns and leaves the
guest with whatever resolution it already had. `jm doctor` reports the loss
and `resolver.log` says why; `jm inspect` shows `Resolver:` and
`Resolver address:`.

> A Linux container that runs its **own** resolver works too: UDP sockets
> bind, send and receive normally under the Linuxulator, so a container
> talking to `/etc/resolv.conf` itself gets the same answers the guest's
> `local_unbound` would give it. See
> [UDP from a container](#udp-from-a-container) for the one idiom that does
> not work.

---

# Autostart

`jpodman` and `jdocker` (and `jm podman` / `jm docker`) **start a stopped
machine on demand**, printing one line on stderr while it boots, then run
your command. A warm start is about 25 s, almost all of it guest boot.

```bash
jm stop
jpodman ps            # "starting jailmachine "jailmachine"..." then podman's output
```

That is the whole mechanism. There is deliberately **no login agent and no
`jm autostart` command**: `jm start` is one-shot and leaves qemu, gvproxy,
the forwarder and the resolver detached, so a launchd `KeepAlive` agent would
loop. Nothing starts a machine unless you, or a wrapper, ask.

| Opt out | Scope |
|---|---|
| `jpodman --no-autostart ps` | One invocation; recognised only as the **first** argument, so it cannot be mistaken for an argument of the container command |
| `JM_AUTOSTART=0` (`false`, `no`, `off`) | The environment |
| `JM_NO_AUTOSTART=1` | The same, spelt the other way round |

With autostart off, a stopped machine is an error naming the `jm start` that
would fix it. Concurrent wrappers are safe: the start waits on the
per-machine lock rather than failing, so the second of two racing wrappers
finds the machine running by the time it gets in.

`jm inspect` shows `Autostart: on` or `off ($JM_AUTOSTART)`.

---

# Publishing ports and `--publish-addr`

`-p` works as on any other machine, and the address you write in it means
what it means under Docker Desktop: **the address on your Mac**. The
detached forwarder watches podman events and converges gvproxy's mapping
table onto the guest's containers (ADR 0004).

| You type | Bound on the Mac |
|---|---|
| `-p 8080:80` | the machine's **publish address** — `0.0.0.0` by default, every interface |
| `-p 0.0.0.0:8080:80` | every interface, whatever the machine's default is |
| `-p 127.0.0.1:8080:80` | your loopback only; the LAN gets connection refused |
| `-p [::1]:8080:80` | your IPv6 loopback only |
| `-p 192.168.0.18:8080:80` | that address only; an address your Mac does not have is a per-mapping error, as under docker |
| `-p 8080-8082:80-82`, `-p 8080:80/udp` | as above, one mapping per port, protocol preserved. A published UDP datagram is capped at 8972 bytes — see [Datagrams are capped at 8972 bytes](#datagrams-are-capped-at-8972-bytes) |

`-p localhost:8080:80` is the one docker spelling that does not reach jm at
all: podman rejects the name client-side.

**The default is `0.0.0.0` — every interface — as `docker run -p` does on
Linux.** `127.0.0.1`, `::1`, `localhost` and your Mac's LAN address all reach
the container, which means **anyone on your network does too**.

```bash
jm init --publish-addr 127.0.0.1        # at creation
jm set --publish-addr 127.0.0.1         # later; applies at the next stop + start
JM_PUBLISH_ADDR=127.0.0.1 jm start      # for this boot, folded into the record
jm ports                                # "# publishing on ..." above the table
```

`--publish-addr` is the **default** for a `-p` that names no address of its
own. It never overrides one that does: on a machine publishing on the LAN,
`-p 127.0.0.1:8080:80` is still your loopback and nothing else.

The address is a property of the **machine**, not of the shell that happened
to boot it. The forwarder runs detached, so a variable read inside it would
be invisible to `jm inspect` and `jm ports` and would change under you the
next time anyone ran a plain `jm start`. `$JM_PUBLISH_ADDR` is therefore
folded into the record at start time, and `jm inspect` shows
`Publish address:`. A running forwarder keeps binding the address it started
with: after `jm set --publish-addr`, `jm ports` and `jm inspect` show what is
really bound and mark the record's new value as waiting for a restart.

### How a host address in `-p` is made to work

The engine inside the guest reads that address as a *guest*-side bind
address: `-p 127.0.0.1:8080:80` makes it redirect the **guest's** loopback,
where nothing on the Mac can reach it. jm therefore does both halves itself —
it binds `127.0.0.1:8080` on the Mac through gvproxy, and loads a redirect of
its own into a pf anchor (`rdr/jm`) inside the guest so that the port is
reachable at the guest's address, which is where gvproxy delivers. The anchor
is rewritten whole on every change, so it is always exactly the current
container set and never accumulates.

Two consequences worth knowing:

- the guest leg is IPv4 even for `-p [::1]:…`: the container network is
  `10.88.0.0/16`. What you observe matches docker; the plumbing does not;
- while the redirect is not in place yet (a container that has just started,
  a guest that could not be reached), the host port is bound but nothing
  answers, and `jm ports` shows the reason on that mapping. The next resync
  fixes it.

## UDP from a container

UDP works, from native FreeBSD containers and Linux ones alike. Binding,
sending, receiving, DNS-over-UDP and publishing with `-p <host>:<port>/udp`
were all verified end to end, reached from the Mac's loopback and from its
LAN address:

```bash
jpodman run -d --name udpecho --os=linux -p 5354:53/udp docker.io/alpine \
  sh -c 'apk add -q socat && exec socat UDP4-RECVFROM:53,fork SYSTEM:"tr a-z A-Z"'

echo "hello from the mac" | nc -u -w3 127.0.0.1 5354   # -> HELLO FROM THE MAC
echo "over the lan"       | nc -u -w3 192.168.0.18 5354 # -> OVER THE LAN
```

`jm ports` lists a udp mapping like any other:

```
LOCAL         REMOTE              PROTO  STATUS
0.0.0.0:5354  192.168.127.2:5354  udp    ok
```

### The one thing that does not work: busybox `nc -u -l`

```
$ jpodman run --rm --os=linux docker.io/alpine sh -c 'nc -u -l -p 9999'
nc: can't connect to remote host: Address family not supported by protocol
```

This is busybox's UDP listener alone, not UDP. To learn who is talking to it
so it can reply, busybox peeks the sender's address with a **zero-length**
`recvmsg()` and then `connect()`s the socket to whatever came back. On Linux
that call blocks until a datagram arrives and fills in the address; on
FreeBSD — as on macOS, and in the guest outside any container — it returns
`0` immediately with no address, so busybox connects to an all-zero sockaddr
and gets `EAFNOSUPPORT`. The Linuxulator inherits the FreeBSD behaviour
verbatim. It is a blocking-semantics gap, not a missing address family: a
`recvmsg()` with even one byte of buffer blocks on all three systems.

Anything that is not that idiom is fine:

```bash
# a real netcat
jpodman run --rm --os=linux docker.io/alpine \
  sh -c 'apk add -q netcat-openbsd && nc -u -l -p 9999'

# socat
jpodman run --rm --os=linux docker.io/alpine \
  sh -c 'apk add -q socat && socat UDP4-RECVFROM:9999,fork SYSTEM:"tr a-z A-Z"'
```

### Datagrams are capped at 8972 bytes

The host-to-guest link is gvproxy's and **it does not fragment**, so the
largest UDP payload that survives is the link MTU less the 20-byte IPv4 and
8-byte UDP headers. The MTU is **9000** by default — the virtio-net jumbo
frame the guest NIC advertises and gvproxy hands out over DHCP — so a
payload of 8972 bytes arrives and 8973 is dropped in silence, with no error
on either side and nothing in any log:

```
1400 bytes -> reply 1400
8972 bytes -> reply 8972
8973 bytes -> no reply
```

TCP never meets this — the stack segments to fit — so it only shows up on
UDP, and the jumbo default costs it nothing (10 MB downloads measured at
2.4–2.8 s with MTU 9000 against 2.9–3.8 s with MTU 1500). `jm doctor` prints
the number for each machine:

```
[ ok ]  datagram limit dev   published udp carries payloads up to 8972 bytes (gvproxy MTU 9000); larger datagrams are dropped, not fragmented. $JM_MTU changes the link size (576..16384; JM_MTU=1500 matches Docker)
```

`$JM_MTU` moves the ceiling. It is read from the environment at `jm start`,
clamped to 576–16384, and the guest picks the value up over DHCP:

```bash
JM_MTU=1500 jm start                # Docker's exact link size, ceiling 1472
jm ssh -- ifconfig vtnet0 | head -1 # what the guest settled on
```

It is not stored in the machine record, so a machine runs with whatever was
in the environment the last time it was started — and `jm doctor` prints the
limit from the same variable rather than from the running machine, so if the
two could differ, `ifconfig vtnet0` in the guest is the authority.

A ceiling remains whatever the MTU: gvproxy never fragments, so this is
**not** Linux-style fragmentation, only a wall six times further out than a
1500-byte link puts it. Design for it as you would for any other network:
keep datagrams under the limit, or use TCP. DNS is unaffected in practice —
a reply that does not fit falls back to TCP, which is exactly what the
truncation bit is for.

### Picking a host port

Choose the host side of a UDP publish with the Mac in mind: macOS runs
mDNSResponder on `5353/udp`, so `-p 5353:53/udp` collides with it and any
other listener on that port. jm reports the collision per mapping rather
than failing the container:

```
0.0.0.0:5353  192.168.127.2:5353  udp  error: another process on this Mac already holds this host port (lsof -nP -iUDP:5353); publish the container on a different host port
```

Publish on a free port instead — `-p 5354:53/udp`. The mapping is retried on
every resync, so freeing the port is enough to make it come up.

---

# Compose and Kubernetes YAML

A multi-container stack reaches a machine by one of three routes. All three
end at the **same engine** — the guest's podman — and every port they publish
is reconciled by the **same forwarder**, so `jm ports` explains any of them.

| Route | Needs on the Mac | Pick it when |
|---|---|---|
| [`jdocker compose`](#docker-compose-through-jdocker) | the `docker` CLI with the Compose plugin | You already have a `compose.yaml`, and want Docker Desktop's behaviour: host bind mounts, `ports:`, a default platform that is already `linux/arm64` |
| [`jpodman compose`](#docker-compose-through-jpodman) | `podman`, plus Compose as its external provider | podman is your client and you would rather not point `DOCKER_HOST` at anything by hand |
| [`jpodman kube play`](#podman-kube-play) | nothing but `podman` | You want the route FreeBSD itself implements — no external provider on the Mac, and the same YAML a FreeBSD server would run |

> **Nothing compose-shaped is installed in the guest.** `podman-compose` is
> not packaged for FreeBSD, and `podman compose` is a shim that hands the work
> to a provider running **on your Mac**. `podman kube play` is the one podman
> implements itself, which is why it is the FreeBSD-native answer — the same
> conclusion the
> [freebsd-oauth2-proxy-oci](https://github.com/gabrielbelli/freebsd-oauth2-proxy-oci)
> image documents for a bare-metal FreeBSD host.

## Docker Compose through `jdocker`

`jm docker` execs the real docker CLI with `DOCKER_HOST` pointing at the
machine's socket, so `docker compose` drives the guest's podman with nothing
else to configure (verified with the Docker Compose plugin v5.3.1). Bind mounts work because host directories exist in the
guest **at the same absolute path** (ADR 0007), so Compose's own path
expansion — relative sources become absolute host paths — lands on the right
directory.

```yaml
# compose.yaml
services:
  web:
    image: docker.io/library/busybox
    platform: linux/arm64
    command: ["httpd", "-f", "-p", "80", "-h", "/www"]
    volumes:
      - ${HOME}/jm-share-test/compose:/www:ro
    ports:
      - "8190:80"
```

```bash
mkdir -p ~/jm-share-test/compose
echo 'served from the Mac' > ~/jm-share-test/compose/index.html
jdocker compose up -d
curl --retry 10 --retry-connrefused http://127.0.0.1:8190/   # served from the Mac
jm ports
jdocker compose down
```

The bind source must be inside a shared root (`jm inspect` lists them, and
`/tmp` is the one path that is not what you think — write
`/private/tmp/...`). Everything else is ordinary Compose: it is talking to
podman's Docker-compatible API over a unix socket, not to a translation
layer.

## Docker Compose through `jpodman`

`podman compose` is a shim: podman looks for an external provider (Docker
Compose, or `podman-compose`) on the host and passes it the connection's URI.
Left alone, podman would hand the provider its **`ssh://`** URI, which sends
Compose looking for a docker daemon inside the guest and fails before
anything starts:

```
command [ssh -l root … docker system dial-stdio] has exited with exit status 255
```

jm answers that in the wrapper: a `compose` invocation is targeted at the
machine's **socket** connection (`<name>-sock`) instead, with `DOCKER_HOST`
and `CONTAINER_HOST` set to that unix socket, which is what the provider
needs. So the plain thing works:

```bash
jpodman compose up -d
jpodman compose ps
jpodman compose down
```

Only `compose` is treated this way; every other `jpodman` invocation still
uses the SSH connection, untouched. It needs a network provider that exposes
an API socket (gvproxy, the default) — without one there is no unix socket to
hand the provider, and the `ssh://` failure above is what you get.

> **One difference from `jdocker`:** the podman wrapper sets no default
> platform, so a service using a Linux image needs `platform: linux/arm64` in
> the file (or a pre-pull — see
> [Linux images need a platform](#linux-images-need-a-platform)).

## `podman kube play`

The FreeBSD-native route: podman reads a Kubernetes `Pod` (or `Deployment`)
manifest and runs it, with no provider on the Mac and no compose
implementation anywhere.

```yaml
# pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: jm-demo
spec:
  containers:
    - name: web
      image: docker.io/library/busybox
      imagePullPolicy: IfNotPresent   # keep the image pulled with --os=linux
      command: ["sh", "-c"]
      args: ["echo hello from a pod > /tmp/index.html && httpd -f -p 80 -h /tmp"]
      ports:
        - containerPort: 80
          hostPort: 8191
```

```bash
jpodman pull --os=linux docker.io/library/busybox   # Linux image: select the manifest first
jpodman kube play pod.yaml
curl --retry 10 --retry-connrefused http://127.0.0.1:8191/   # hello from a pod
jm ports
jpodman kube down pod.yaml
```

A pod needs an **infra container**, whose init process is `catatonit`. The
guest image installs `sysutils/catatonit` alongside `podman-suite`
(`guest/provision.sh`), so `kube play` works on a machine created from a
current image; on an older one, see
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).

`imagePullPolicy` matters here: podman follows Kubernetes' rule that an
image tagged `:latest`, or tagged not at all, is pulled every time — which
would ask the registry for the FreeBSD variant again and undo the
`--os=linux` pull. `IfNotPresent` uses the image already in the guest.

`jpodman kube generate <container|pod>` writes a manifest out of what is
already running, which is the quickest way to a file that works.

## Linux images need a platform

The guest is a FreeBSD host, so podman there picks **FreeBSD image variants**
by default. Neither compose nor `kube play` has an `--os` flag, so the choice
has to be made another way:

| Route | Say it like this |
|---|---|
| `jdocker compose` | Nothing, usually: the wrapper defaults `DOCKER_DEFAULT_PLATFORM=linux/arm64`. `platform: linux/arm64` on the service is the explicit, portable spelling |
| `jpodman compose` | `platform: linux/arm64` on the service |
| `eval "$(jm env)"; docker compose` | `platform: linux/arm64`, or pre-pull plus `pull_policy: missing` |
| `jpodman kube play` | `jpodman pull --os=linux <image>` first; the manifest itself cannot ask |

Without it, the pull fails with the OS in the message:

```
Error response from daemon: no image found in image index for architecture "arm64", variant "", OS "freebsd"
```

The pre-pull form works for every route — put the image in the guest with the
platform already chosen, and tell compose not to fetch it again:

```bash
jpodman pull --os=linux docker.io/library/busybox
```

```yaml
services:
  web:
    image: docker.io/library/busybox
    pull_policy: missing       # use the linux/arm64 image already in the guest
```

Native FreeBSD images (`ghcr.io/freebsd/freebsd-runtime:15.1` and friends)
need none of this — and under `jdocker` they need
`DOCKER_DEFAULT_PLATFORM=freebsd/arm64`, or an explicit `platform:`, to
override the wrapper's default.

## Ports go through the same forwarder

A compose `ports:` entry and a `hostPort:` in a Pod manifest are both just
published ports on the guest's engine. The detached forwarder watches the
guest's container state and converges gvproxy's mapping table onto it, so
they behave exactly like `-p` on `jpodman run`:

```
# publishing on 0.0.0.0 unless -p names a host address
LOCAL           REMOTE              PROTO  STATUS
0.0.0.0:8190    192.168.127.2:8190  tcp    ok
0.0.0.0:8191    192.168.127.2:8191  tcp    ok
```

That means the same rules apply: the default host address is the machine's
publish address (`0.0.0.0` — every interface, including your LAN), the
mapping appears a second or two after the container starts, and a failure is
reported per mapping rather than failing the stack. See
[Publishing ports and `--publish-addr`](#publishing-ports-and---publish-addr).

## Healthchecks and restart policies do not fire

**Anything long-lived started from a compose file or a Pod manifest is
affected**: podman on FreeBSD has no systemd timers, so a `healthcheck:` /
`livenessProbe` never runs on a schedule (the status sits at `starting`), and
`restart: always` applies only at boot, not when a process dies. That is a
podman-on-FreeBSD gap, not a jm one — a bare-metal FreeBSD container host
behaves the same way — and it is tracked as
[#3](https://github.com/gabrielbelli/jailmachine/issues/3).

Run one by hand, or from a cron entry in the guest:

```bash
jm ssh -- podman healthcheck run web
jm ssh -- podman ps --format '{{.Names}}  {{.Status}}'
```

Do not design a stack that depends on the engine restarting a crashed
container for you until that is fixed.

---

# Recipes

## Build and run a native FreeBSD image

Native FreeBSD images need no `--os` flag and hit none of the Linuxulator
limits. Use the FreeBSD project's images as a base — note that
`freebsd15-minimal` is for static binaries and carries no `pkg` runtime.

```Dockerfile
FROM ghcr.io/freebsd/freebsd-runtime:15.1
RUN env ASSUME_ALWAYS_YES=yes pkg bootstrap && pkg install -y curl && pkg clean -ay
COPY hello.sh /usr/local/bin/hello
CMD ["/usr/local/bin/hello"]
```

```bash
jpodman build -t myapp .
jpodman run --rm myapp
jpodman run --rm docker.io/dougrabson/freebsd15-minimal uname -srm
```

## Run a Linux image

```bash
jpodman run --rm --os=linux docker.io/alpine echo hi
jpodman pull --os=linux docker.io/busybox      # or set it once at pull time
```

Almost everything on Docker Hub works, including `nginx` (with one config
line) and `redis` (with one flag). The verified matrix, and the one image
that does not work, are in
[Docker Hub compatibility](#docker-hub-compatibility-verified).

## Publish a port

`-p` works as on any other machine: the detached forwarder watches podman
events and maps the port through gvproxy.

```bash
jpodman run -d --os=linux -p 8080:80 --name web docker.io/busybox \
  sh -c 'echo hello from the FreeBSD VM > /tmp/index.html && httpd -f -p 80 -h /tmp'
curl --retry 10 --retry-connrefused http://localhost:8080/
jm ports                     # what is mapped, and why something is not
```

The forwarder reconciles a second or two after the container starts, hence
`--retry --retry-connrefused`; a bare `curl` fired immediately usually gets
one connection refused first. `httpd` needs `-h` and an index file, or it
answers `404` rather than a page.

A plain `-p 8080:80` binds the machine's publish address, `0.0.0.0` by
default — so `curl http://localhost:8080/` works, and so does anyone else on
your network. `jm set --publish-addr 127.0.0.1` changes that default, and
naming an address in the publish itself — `-p 127.0.0.1:8080:80` — confines
that one mapping to your Mac's loopback, exactly as under Docker Desktop. See
[Publishing ports and `--publish-addr`](#publishing-ports-and---publish-addr).

## Run a jail with bastille

`bastille` is installed and configured in the guest (ZFS, a `bastille0`
loopback, NAT through `pf`). Jails are reached through `jm ssh`; host-side
jail management (`jm jail ...`) is deliberately out of scope for the MVP.

```bash
jm ssh -- bastille bootstrap 15.1-RELEASE
jm ssh -- bastille create demo 15.1-RELEASE 10.17.89.10
jm ssh -- bastille cmd demo pkg install -y curl
jm ssh -- bastille list
```

## Resize the disk

```bash
jm set --disk 128            # grows disk.raw and, on a running machine, the pool
jm inspect | grep Disk
```

Growing works while the machine runs: the hypervisor is told about the new
size over QMP and the guest's partition and ZFS pool are extended over SSH.
On a stopped machine the guest side happens at the next `jm start`. Disks
only grow — shrinking is a usage error.

CPUs and memory need a stop:

```bash
jm stop && jm set --cpus 8 --memory 8GiB && jm start
```

## Read the serial console

```bash
jm console                   # last 50 lines
jm console -n 200
jm console -f                # follow a boot, Ctrl-C to stop
```

This is the log to read when `jm start` fails at the `backend` or `ssh`
stage — the guest may be sitting at a loader prompt or panicking, neither of
which is visible over SSH.

## Read the provisioning log

```bash
jm ssh -- cat /var/log/jm-provision.log
jm ssh -- tail -f /var/log/jm-provision.log
```

The marker `/var/db/jm-provisioned` means provisioning finished;
`/var/db/jm-provision-failed` means the script aborted, and it is terminal
for that disk — re-create the machine (`jm rm && jm init && jm start`).

## Start from a clean slate

```bash
jm rm --force && jm init && jm start
```

---

# Docker Hub compatibility (verified)

Measured on this Mac against a running machine: host podman 6.1.0, guest
FreeBSD 15.1-RELEASE-p2 arm64, `compat.linux.osrelease=5.15.0`.
`demo/hub-matrix.sh` re-runs the whole matrix and prints this table.

Two rules cover every row:

- **Linux images need `--os=linux`** for `pull` and `build`, and therefore
  for the first `run`. The guest is a FreeBSD host, so podman there defaults
  to `freebsd/arm64`.
- **FreeBSD images need no flag**, whether they come from Docker Hub or from
  GHCR.

## Linux images, straight from Docker Hub

| Image | Checked with | Result |
|---|---|---|
| `docker.io/library/alpine:latest` | `uname -sm` → `Linux aarch64` | Works |
| `docker.io/library/debian:trixie-slim` | `uname -sm` | Works |
| `docker.io/library/ubuntu:24.04` | `uname -sm` | Works |
| `docker.io/library/python:3-alpine` | `python3 --version` | Works |
| `docker.io/library/golang:alpine` | `go version` → `go1.27.0 linux/arm64` | Works |
| `docker.io/library/hello-world:latest` | its own banner | Works |
| `docker.io/library/caddy:alpine` | `caddy version` → 2.11.4 | Works |
| `docker.io/library/postgres:17-alpine` | `postgres --version` → 17.11 | Works |
| `docker.io/library/redis:alpine` | `redis-server --version` → 8.10.1 | Works — but `redis-server` needs one flag to start, see [redis](#redis-one-flag) |
| `docker.io/library/nginx:1.31-alpine` | HTTP 200 through a published port | Works — with one config line, see [nginx](#nginx-one-config-line) |
| `docker.io/library/node:22-alpine` | `node --version` → `v22.23.2` | **Broken**, see [node](#node-the-one-known-bad-image) |

## FreeBSD images, no flag

They come from Docker Hub *and* from GHCR, and neither needs `--os`:

| Image | Registry | Checked with | Result |
|---|---|---|---|
| `docker.io/dougrabson/freebsd15-minimal` | Docker Hub | `uname -so` → `FreeBSD arm64` | Works |
| `docker.io/dougrabson/freebsd14-minimal` | Docker Hub | `uname -so` → `FreeBSD arm64` | Works |
| `ghcr.io/freebsd/freebsd-runtime:15.1` | GHCR | `freebsd-version` → `15.1-RELEASE` | Works |
| `ghcr.io/freebsd/freebsd-runtime:14.3` | GHCR | `freebsd-version` → `14.3-RELEASE` | Works |

## nginx: one config line

Stock `docker.io/nginx` fails as shipped — every worker dies at startup:

```
[alert] 8231#8231: epoll_ctl(1, 6) failed (22: Invalid argument)
[alert] 8154#8154: worker process 8231 exited with fatal code 2 and cannot be respawned
```

The cause is **not** Linux AIO. Since 1.11.3 nginx defaults to
`accept_mutex off`, and with `worker_processes > 1` it registers the
*listening* socket with `EPOLLEXCLUSIVE` so its workers do not
thundering-herd on `accept()`. FreeBSD's `linux_epoll` does not implement
`EPOLLEXCLUSIVE`; it returns `EINVAL`, and nginx treats that as fatal.

One line in the `events` block fixes it:

```nginx
events {
    accept_mutex on;        # nginx serialises accept() itself; no EPOLLEXCLUSIVE
    worker_connections 1024;
}
```

`worker_processes 1;` and `reuseport` on the `listen` directive are the two
other ways out. Measured here (nginx 1.31.4, same config otherwise):

| `worker_processes` | `accept_mutex` | Result |
|---|---|---|
| 4 | `off` (the default) | workers die, connection refused |
| 4 | **`on`** | **HTTP 200, all four workers alive** |

The other line people quote,

```
[emerg] 12097#12097: io_setup() failed (38: Function not implemented)
```

is a **harmless one-off**: nginx probes for Linux AIO, does not find it,
disables file AIO and serves normally. It cannot be configured away, and it
is not what kills the workers.

There is no speed penalty either. 1000 sequential requests took **1.55 s**
against Linux nginx under the Linuxulator and **1.56 s** against a native
FreeBSD nginx on the same guest.

A ready-made image is in `demo/nginx-linuxulator/` and published as
`ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator`:

```bash
jpodman run -d --os=linux -p 8080:80 ghcr.io/gabrielbelli/jm-demo-nginx-linuxulator
curl --retry 10 --retry-connrefused http://localhost:8080/healthz    # ok
```

The whole finding, with the measurements, is in
[demo/README.md](../demo/README.md#the-nginx-finding) and in the comments of
`demo/nginx-linuxulator/nginx.conf`.

## redis: one flag

`redis-server` starts, then aborts:

```
Failed to test the kernel for a bug that could lead to data corruption
during background save … Redis will now exit.
```

Redis' own documented switch skips that probe:

```bash
jpodman run -d --os=linux -p 6379:6379 docker.io/library/redis:alpine \
    redis-server --ignore-warnings ARM64-COW-BUG
printf 'PING\r\n' | nc 127.0.0.1 6379      # +PONG, from the Mac
```

Verified end to end, including the published port.

## node: the one known-bad image

`docker.io/library/node:22-alpine` does not work, and there is no known
workaround for it.

| Symptom | Detail |
|---|---|
| The binary does start | `node --version` prints `v22.23.2` |
| JavaScript output never arrives | Anything written with `console.log` never reaches the pipe |
| Sockets never answer | An HTTP server started inside the container does not accept connections — `curl` gets `connection reset by peer`, both from inside the guest and through a published port |
| `UV_USE_IO_URING=0` | Changes nothing |

Use another runtime image (`python:3-alpine` and `golang:alpine` both work),
or a native FreeBSD image, until this is fixed.

## Re-running the matrix

```bash
demo/hub-matrix.sh            # needs a started machine; prints the table above
```

Each image is run with `timeout 120`, its exit status captured from the
`jpodman run` itself, and the result printed as `ok` or `FAIL`.

---

# What this does not do (yet)

Stated plainly, so you can plan around it.

**Known limits, narrow ones:**

| Limit | What it looks like / what instead |
|---|---|
| busybox `nc -u -l` in a Linux container | Fails with `Address family not supported by protocol`. It is the only known casualty of FreeBSD returning at once from a zero-length `recvmsg()` where Linux blocks. UDP itself works — `apk add netcat-openbsd`, `socat`, or any real UDP server. See [UDP from a container](#udp-from-a-container) |
| UDP datagrams over 8972 bytes | Dropped in silence: the gvproxy link does not fragment, and its MTU is 9000 by default. `jm doctor` states the limit per machine, and `JM_MTU` at `jm start` moves it (576–16384; `JM_MTU=1500` is Docker's link size and its 1472-byte cap). Keep datagrams under the limit, or use TCP. See [Datagrams are capped at 8972 bytes](#datagrams-are-capped-at-8972-bytes) |

**Not planned for the MVP:**

| Not here | Why / what instead |
|---|---|
| Autostart at login | Deliberate: `jpodman`/`jdocker` start a stopped machine on demand and nothing else does. See [Autostart](#autostart) |
| Full POSIX semantics on a share | 9p, not virtiofs: `utimes` is a no-op, guest ownership and modes live in host xattrs, there are no `inotify` events, and it is far slower than ZFS (~70 MB/s). See [What 9p does not do](#what-9p-does-not-do) |
| `--os=linux` per service under compose | Compose cannot ask for a platform. Under `jdocker` the wrapper's default platform covers it; under a plain `eval "$(jm env)"` shell, pre-pull with `jpodman pull --os=linux <image>` plus `pull_policy: missing` |
| `docker.io/node` | The one Docker Hub image known not to work under the Linuxulator; see [Docker Hub compatibility](#docker-hub-compatibility-verified) |
| A routable VM IP | gvproxy is NAT; vmnet/bridged networking is a later step |
| vsock | The podman socket is forwarded over SSH instead |
| Host-side jail management | Jails are reached through `jm ssh -- bastille ...` |
| In-place guest upgrades | Re-create the machine to move to a new guest image |
| Intel Macs, Linux and Windows hosts | Only `darwin/arm64` has a backend; the Linux release binaries are build-only |
