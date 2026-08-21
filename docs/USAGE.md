# Using jailmachine

> **This release is an MVP — a working demo.** Everything documented on this
> page is implemented and verified against the binary. What is *not* here:
> host directory mounts at identical paths, DNS 1:1 with the host, autostart
> on login, and full docker CLI parity. Those are being built on the
> `docker-parity` branch and are not part of this release.

Install first: [INSTALL.md](INSTALL.md). When something misbehaves, see
[TROUBLESHOOTING.md](TROUBLESHOOTING.md); for how the pieces fit together,
[ARCHITECTURE.md](ARCHITECTURE.md).

## The shape of it

```bash
jm init      # create a machine: ssh key, verified guest image, first-boot seed
jm start     # boot it, provision it, register podman connections, publish ports
jpodman run --rm --os=linux docker.io/alpine echo hi
```

- One machine is one FreeBSD VM under `~/.jailmachine/machines/<name>/`.
- The guest is FreeBSD, so podman pulls **FreeBSD image variants by
  default**. Linux images run through the Linuxulator and need `--os=linux`
  (or `podman pull --os=linux`).
- Host and guest talk over SSH only. There is no host directory sharing in
  this release (no virtiofs driver in the FreeBSD guest; 9p is what the
  parity branch is building) and no vsock.

## Conventions shared by every command

| | |
|---|---|
| `[name]` | Optional machine name. Defaults to `jailmachine`; if that does not exist and exactly one machine does, that one is used (jm says so). With several machines and no `jailmachine`, you must name one — that is a usage error |
| Exit codes | `0` success, `1` failure, `2` usage error (bad flag, bad name, ambiguous default). `jm ssh` and `jm podman` replace themselves with `ssh`/`podman`, so their exit code is that program's |
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

```bash
jm init
jm init --cpus 2 --memory 2048 dev
jm init --image official:15.1-RELEASE --disk 32
```

Exit codes: `0` created; `2` for an invalid flag value or a bad machine
name; `1` for anything else (image download or checksum failure, the name
already exists, a missing host tool).

### Image sources

| `--image` | What you get | Verification | `image_trusted` |
|---|---|---|---|
| `prebaked` (default) | An already-provisioned guest published on this repo's `guest-<version>` GitHub release — this release publishes `guest-15.1.0`. First boot is a boot, nothing more (about 22 s cold) | mandatory `.sha256` sidecar next to the asset, verified by `jm init` | `true` |
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
provider) → **backend** (boot the hypervisor) → **ssh** (wait for sshd) →
**provision** (wait for the guest's ready marker) → **connect** (register the
podman connections) → **forwarder** (start the detached port-publishing
loop).

Starting a machine that is already running re-checks the ssh, provision,
connect and forwarder stages, so an interrupted start finishes rather than
needing a stop first. A *broken* machine (half of it running, or a stale pid
file) is stopped and started again.

| Flag | Effect |
|---|---|
| `--set-default` | Also make this machine the **default** podman connection. Without it, jm registers the connections but never repoints a default you already had (see [Podman connections](#podman-connections)) |

```bash
jm start
jm -q start && jpodman run --rm --os=linux docker.io/alpine echo hi
```

On failure the error names the stage and the log to read: `qemu.log` and
`console.log` (hypervisor), `gvproxy.log` and `forward.log` (networking),
`forwarder.log` (port publishing), or `/var/log/jm-provision.log` inside the
guest.

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
DNS:            192.168.127.1
SSH:            root@127.0.0.1:2222
SSH key:        /Users/you/.jailmachine/machines/jailmachine/ssh/id_ed25519
Podman:         ssh://root@127.0.0.1:2222/var/run/podman/podman.sock (jailmachine)
Podman socket:  unix:///Users/you/.jailmachine/machines/jailmachine/podman.sock (jailmachine-sock)
Console:        /Users/you/.jailmachine/machines/jailmachine/console.log
Network log:    /Users/you/.jailmachine/machines/jailmachine/gvproxy.log
Network log:    /Users/you/.jailmachine/machines/jailmachine/forward.log
Forwarder:      running
Forwarder log:  /Users/you/.jailmachine/machines/jailmachine/forwarder.log
Port:           127.0.0.1:8080 -> 192.168.127.2:8080 tcp (ok)
Dir:            /Users/you/.jailmachine/machines/jailmachine
Provisioned:    true
Created:        2026-08-21T02:37:54Z
```

One `Port:` line is printed per published mapping, between `Forwarder log:`
and `Dir:`; a machine publishing nothing has none.

`--json` prints one object with snake_case keys: `name`, `state`
(`running` | `stopped` | `broken`), `backend_state`, `network_state`,
`backend`, `network`, `image`, `cpus`, `memory_mib`, `disk_gib`, `mac`,
`ssh_port`, `ssh_user`, `guest_ip`, `ssh` (host:port), `ssh_key`,
`podman_uri`, `podman_sock_uri`, `api_socket`, `dns`, `console`,
`network_logs`, `dir`, `provisioned`, `image_trusted`, `created`, `version`,
`backend_opts`, `ports` (array of `{proto, local, remote, since, error}`),
`forwarder_state`, `forwarder_log`. Empty values are omitted.

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
LOCAL           REMOTE              PROTO  STATUS
127.0.0.1:8080  192.168.127.2:8080  tcp    ok
127.0.0.1:5432  192.168.127.2:5432  tcp    error: listen tcp 127.0.0.1:5432: address already in use
127.0.0.1:7070  -                   tcp    error: guest binds 127.0.0.1 only; publish with -p 7070:7070 (or 0.0.0.0)
```

`LOCAL` is the host side. A publish that names no host address binds the
host's loopback, `127.0.0.1`. `REMOTE` is the guest side.

`STATUS` is `ok`, or `error: <cause>` — a host port already in use, a
provider that could not be reached, or a publish that cannot be forwarded at
all. Failed mappings are retried on every resync, so the error is a live
status, not a permanent verdict.

A `REMOTE` of `-` is a mapping that is never forwarded, which is what
`-p 127.0.0.1:7070:7070` looks like: podman bound it to the **guest's**
loopback, so there is nothing outside the guest to forward to. Publish
without a host address (or to `0.0.0.0`) instead.

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

`--disk` extends `disk.raw` sparsely. On a running machine the guest's
partition and ZFS pool are extended immediately; on a stopped one they are
extended on the next `jm start`. If the guest side fails, the record keeps a
pending flag and the next start retries it.

```bash
jm stop && jm set --cpus 8 --memory 8GiB && jm start
jm set --disk 128            # works while running
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

## `jm podman [podman args...]` (and `jpodman`)

Run the host `podman` client against a machine without touching your default
podman connection: jm execs `podman --connection <machine> <your args>`.

Machine selection: `$JM_MACHINE` if set, otherwise the usual default
resolution. Flag parsing **stops at the first positional argument**, so a
podman subcommand and everything after it is passed through untouched:

```bash
jm podman run --rm --os=linux docker.io/alpine echo hi
jpodman build -t myapp .
jpodman ps --help                 # podman's own help for ps
JM_MACHINE=dev jpodman ps
```

Podman flags placed **before** the subcommand are jm's to parse, and jm
rejects them with exit `2`:

| You type | What happens | Instead |
|---|---|---|
| `jpodman --version`, `jpodman -v` | `jm: podman: unknown flag: --version`, exit 2 | `jpodman version` |
| `jpodman --log-level debug ps`, `jpodman --remote …` | same, exit 2 | put the flag after the subcommand, or run `podman --connection <name> …` directly |
| `jpodman --help`, `jpodman -h` | jm's help for the `podman` subcommand, not podman's | `jpodman ps --help` for a subcommand; `podman --help` for podman's own |

With several machines and none named `jailmachine`, `jpodman` cannot pick
one and the error suggests naming it as `jm podman <name>` — **that hint is
wrong for this command**, which takes no machine-name argument. Select the
machine with the environment variable instead:

```bash
JM_MACHINE=dev jpodman ps
```

`jpodman` is a symlink to `jm` — invoked under that name, `jpodman X` runs
`jm podman X`. The exit code is podman's own.

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

## `jm _forwarder` (internal)

The hidden foreground entry point of the port-publishing loop. `jm start`
launches it detached and `jm stop` terminates it; there is no reason to run
it by hand. It is documented here only so you recognise it in `ps` output
and in `forwarder.log`.

---

# Environment variables

Every one of these is read by the binary; there is no `JM_SSH_PORT` — the
SSH port is a flag (`jm init --ssh-port`, `jm set --ssh-port`) stored in the
machine record.

| Variable | Read by | Effect |
|---|---|---|
| `JM_HOME` | all commands | State root, same as `--state-root`. The flag wins if both are given. Default `~/.jailmachine` |
| `JM_MACHINE` | `jm podman` / `jpodman` | Which machine to talk to, instead of the default resolution |
| `JM_NETWORK` | `jm init` (recorded), providers | Network provider to create a machine with: `gvproxy` (default) or `user` (QEMU slirp: no `jm env`, no port publishing). Fixed at init — a machine keeps the networking it was created with |
| `JM_BACKEND` | `jm init` (recorded), backends | Force a backend name. Needed on Linux, where there is no default backend yet (`JM_BACKEND=qemu`) |
| `JM_QEMU_ACCEL` | QEMU backend | Accelerator override. Default `hvf` on macOS, `kvm` on Linux; `JM_QEMU_ACCEL=tcg` is pure emulation with `-cpu cortex-a72`, an order of magnitude slower — for building images on machines without a hypervisor, not for using them. Start-stage timeouts are stretched eightfold under TCG |
| `JM_IMAGE_BASEURL` | prebaked image source | Fetch the prebaked image and its `.sha256` from `$JM_IMAGE_BASEURL/<file name>` instead of the GitHub release. For testing an image that is not published yet |
| `JM_GVPROXY` | gvproxy provider | Path to the `gvproxy` binary, instead of `PATH` and then `/opt/homebrew/opt/podman/libexec/podman/gvproxy` |
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

`jm env` exports `DOCKER_HOST` (and `CONTAINER_HOST`) pointing at the same
unix socket, which is all the docker CLI and docker-compose need:

```bash
eval "$(jm env)"
docker ps
docker version               # server: freebsd/arm64/freebsd-15.1
docker compose up -d         # FreeBSD images only — see below
```

**Compose and Linux images.** The guest is a FreeBSD host, so compose pulls
FreeBSD image variants, and compose has no per-service equivalent of
`--os=linux`. A service using a Linux image fails at pull time with:

```
Error response from daemon: no image found in image index for architecture "arm64", variant "", OS "freebsd"
```

Pre-pull the Linux image and tell compose not to pull again:

```bash
jpodman pull --os=linux docker.io/busybox
```

```yaml
services:
  web:
    image: docker.io/busybox
    pull_policy: missing       # use the linux/arm64 image already in the guest
    ports: ["8082:80"]
    command: ["httpd", "-f", "-p", "80"]
```

With those two lines `docker compose up -d` succeeds, `jm ports` shows the
mapping as `ok` and the port answers on the Mac. Native FreeBSD images
(`ghcr.io/freebsd/freebsd-runtime:15.1` and friends) need neither step.

This is socket-level compatibility, not CLI parity: the docker CLI talks to
podman's API, and there is no `jdocker` wrapper to match `jpodman`. Fuller
docker parity is what the `docker-parity` branch is about, and it is not in
this release.

## Multiple machines

Machines are fully independent — their own disk, key, ports and podman
connections. Give each one its own SSH port.

```bash
jm init --cpus 2 --memory 2048 --ssh-port 2223 dev
jm start dev
jm list

JM_MACHINE=dev jpodman ps          # dev's engine
jm ssh dev -- uname -a
jm stop dev
```

With several machines and no machine literally named `jailmachine`, a
command without a name is a usage error listing the candidates.

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

A plain `-p 8080:80` ends up bound to the host's loopback
(`127.0.0.1:8080`), which is what `curl http://localhost:8080/` wants.
Naming an address in the publish — `-p 127.0.0.1:8080:80` — instead binds
the **guest's** loopback, and `jm ports` reports the mapping with an empty
`REMOTE` and an error rather than forwarding it.

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

`docker.io/library/node:22-alpine` does not work, and this release has no
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

# What this release does not do

Stated plainly, so you can plan around it:

| Not in this release | Why / what instead |
|---|---|
| Host directory mounts (`-v /host/path:/same/path`) | The FreeBSD guest has no virtiofs driver. 9p support is what the `docker-parity` branch is building. For now, keep volumes inside the VM, or move files with `jm ssh` / `podman cp` / NFS / sshfs |
| DNS resolving 1:1 with the host | Guest DNS is gvproxy's (`192.168.127.1`) |
| Autostart on login | Run `jm start` yourself |
| Full docker CLI parity, and a `jdocker` wrapper | `jm env` gives socket-level compatibility with the docker CLI and compose; there is no docker-named twin of `jpodman` |
| `--os=linux` per service under compose | Compose cannot ask for a platform, so a Linux image needs `jpodman pull --os=linux <image>` first plus `pull_policy: missing` |
| `docker.io/node` | The one Docker Hub image known not to work under the Linuxulator; see [Docker Hub compatibility](#docker-hub-compatibility-verified) |
| A routable VM IP | gvproxy is NAT; vmnet/bridged networking is a later step |
| vsock | The podman socket is forwarded over SSH instead |
| Host-side jail management | Jails are reached through `jm ssh -- bastille ...` |
| In-place guest upgrades | Re-create the machine to move to a new guest image |
