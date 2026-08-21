# Installing jailmachine

> **Still an MVP — a working demo**, but a wider one than v0.1.0. One command
> brings up a FreeBSD VM, native FreeBSD OCI images and Linux images both run,
> published ports reach the host, and bastille jails work inside the guest.
> Since v0.1.0 it also shares host directories at identical paths, resolves
> names exactly as the Mac does, starts a stopped machine on demand from
> `jpodman`/`jdocker`, ships a `jdocker` wrapper for the docker CLI, and
> publishes ports with docker-identical `-p` semantics, UDP included. The
> one Linuxulator gap left is narrow: busybox's `nc -u -l` fails where every
> other UDP server works — see
> [TROUBLESHOOTING](TROUBLESHOOTING.md#udp-in-a-linux-container).

See [USAGE.md](USAGE.md) for the command reference once jm is installed,
[TROUBLESHOOTING.md](TROUBLESHOOTING.md) when something misbehaves, and
[ARCHITECTURE.md](ARCHITECTURE.md) for how the pieces fit together.

## Requirements

| Requirement | Detail |
|---|---|
| Host | macOS 14 or newer on **Apple Silicon** (arm64). `jm doctor` fails the `host` check on anything else; Linux and Windows backends are planned, and the Linux binaries in the release are build-only |
| Virtualisation | Hypervisor.framework (HVF), used through QEMU. Nested VMs (a Mac that is itself a VM, most cloud macOS runners) cannot run jailmachine |
| QEMU | 8.0 or newer (`brew install qemu`) — provides `qemu-system-aarch64` and the EDK2 firmware |
| podman | 5.0 or newer (`brew install podman`) — the host client, and the `gvproxy` binary jm uses for networking ships inside the podman formula at `libexec/podman/gvproxy` |
| OpenSSH | `ssh` and `ssh-keygen` from the base system (`xcode-select --install` if missing) |
| RAM | About 4 GB free: the default machine is 4 vCPU / 4096 MiB |
| Disk | About 10 GB free. The image download is roughly 800 MiB compressed; `disk.raw` is created sparse at the `--disk` size (64 GiB by default) and only occupies what the guest actually writes |
| Optional | `xz` (`brew install xz`) makes decompression of the official image faster; without it jm falls back to a slower in-process decoder. `zstd` is needed only by maintainers running `jm image build` |

## Install

Three supported paths. They differ in what gets installed for you.

| Path | Gives you | Installs dependencies | `jpodman` / `jdocker` symlinks | Gatekeeper |
|---|---|---|---|---|
| Homebrew cask | `jm` on `PATH` | Yes (`qemu`, `podman`) | Yes | Handled for you |
| `go install` | `jm` in `$(go env GOPATH)/bin` | No | Manual | May need `xattr` |
| From source | `jm` in `$PREFIX/bin` | No | Yes (`make install`) | Locally built, so not quarantined |

### 1. Homebrew cask (recommended)

```bash
brew install --cask gabrielbelli/tap/jailmachine
```

The cask declares `qemu` and `podman` as dependencies, so Homebrew installs
them if they are missing. It also strips the quarantine flag from the binary
and creates the `jpodman` and `jdocker` symlinks next to `jm`. The docker CLI
itself is not a dependency — `brew install docker` if you want `jdocker` or
`docker compose`.

`brew install gabrielbelli/tap/jailmachine` (without `--cask`) resolves to
the same cask.

### 2. `go install`

```bash
brew install qemu podman
go install github.com/gabrielbelli/jailmachine/cmd/jm@latest
```

A binary installed this way reports the module version it was built from
(`jm version` shows `0.1.1`, with an empty commit when the module proxy built
it); the Homebrew cask and the release archives carry the commit and build date
as well. `jpodman` and `jdocker` are not created for you: link them yourself (see
below).

Then put the Go bin directory on your `PATH` and create the wrapper symlinks
by hand:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
ln -sf "$(go env GOPATH)/bin/jm" "$(go env GOPATH)/bin/jpodman"
ln -sf "$(go env GOPATH)/bin/jm" "$(go env GOPATH)/bin/jdocker"
```

`jm` decides it is being called as a wrapper from the name of the binary, so
the links must be called exactly `jpodman` and `jdocker`.

> A binary built by `go install` is not stamped with a version: `jm version`
> reports `dev`, `none`, `unknown`. That is expected, and it is how jm tells
> a development build from a release.

### 3. From source

```bash
git clone https://github.com/gabrielbelli/jailmachine
cd jailmachine
make install
```

`make install` builds with the version, commit and date stamped from `git
describe` and installs `jm` and both the `jpodman` and `jdocker` symlinks into
`$(PREFIX)/bin`. `PREFIX` defaults to `/opt/homebrew`:

```bash
make install PREFIX="$HOME/.local"
```

`make build` instead leaves `./jm` in the working tree, which is enough to
try it out.

## Verify the install

```bash
jm doctor
```

Every host tool, the state root and every existing machine is checked, with
a one-line fix for anything that is not `[ ok ]`. Real output on a healthy
Mac:

```
jm dev (none, unknown)

STATUS  CHECK                        DETAIL
[ ok ]  host                         darwin/arm64
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

13 ok, 0 warning(s), 0 failure(s)
```

The first line is the build identity. `jm dev (none, unknown)` means an
unstamped build (`go install`, plain `go build`); a Homebrew or `make
install` build prints its tag, short commit and build date there instead.

The `machine …` rows only appear once you have created a machine. Failures
carry a `fix:` line under the row:

```
[FAIL]  podman version               4.9.0 at /opt/homebrew/bin/podman
        fix: brew upgrade podman (need 5.x or newer)
```

`jm doctor` exits `1` if any check failed; warnings alone still exit `0`.
`jm doctor --json` prints the same report as one object for scripting.

> `jm doctor` inspects the default state root (`~/.jailmachine`, or
> `$JM_HOME`) unless you pass `--state-root`.

## First run

```bash
jm init      # download the prebaked guest image, verify it, write the seed
jm start     # boot, provision, connect podman, mount the shares, start the forwarder and resolver
jpodman run --rm --os=linux docker.io/alpine echo hi
```

Measured on an Apple Silicon Mac (2026-08-21):

| | Time |
|---|---|
| `jm init` from the published release | about 45–60 s on a fast link, almost all of it the download |
| First boot, prebaked image (the default) | about 22 s (32 s measured once with two other VMs already running on the host) |
| First boot, `--image official:<release>` | about 2 min (packages are installed inside the guest, including one kernel reboot) |
| Later starts of an existing machine | 12–25 s |

Most of that is the guest's own boot, so a later start costs much the same
as the first one on the prebaked path; a busy host adds to both. `jm init`
is separate and dominated by the download — it fetches the
`guest-<version>` release asset named by the binary, roughly 800 MiB, then verifies its SHA256 and
decompresses it sparsely.

The guest is FreeBSD, so podman pulls FreeBSD image variants by default.
**Linux images need `--os=linux`**; native FreeBSD images need nothing, and
they come from Docker Hub as well as GHCR. Almost everything on Docker Hub
works — the verified matrix, including the two images that need a
workaround and the one that does not work at all, is in
[USAGE.md](USAGE.md#docker-hub-compatibility-verified).

Docker clients and compose reach the machine through `jdocker` (`brew install
docker` for the client), or with `eval "$(jm env)"` for a client you would
rather point yourself. `jdocker` defaults `DOCKER_DEFAULT_PLATFORM` to
`linux/arm64`, so a plain `jdocker run` pulls the Linux image as Docker
Desktop would; export the variable yourself, or pass `--platform`, for native
FreeBSD images. Under plain `podman`, compose still needs
`jpodman pull --os=linux <image>` plus `pull_policy: missing` for a Linux
image, because it cannot ask for a platform itself.

Both wrappers start a stopped machine before running the client, printing one
line on stderr while it boots. `JM_AUTOSTART=0` (or `--no-autostart` as the
first argument) makes them fail instead. There is no login agent: nothing
starts a machine unless you or a wrapper asks.

Host directories are shared into the guest at the same absolute path — your
home tree, `/Volumes`, `/private/tmp` and `$TMPDIR`'s root by default — so
`-v ~/code:/app` works out of the box. `jm init --mount`, `--no-mounts` and
`jm set --mount/--unmount` change the set; see
[USAGE.md](USAGE.md#sharing-host-directories).

## Upgrade

| Installed with | Upgrade |
|---|---|
| Homebrew cask | `brew update && brew upgrade --cask jailmachine` |
| `go install` | `go install github.com/gabrielbelli/jailmachine/cmd/jm@latest` |
| From source | `git pull && make install` |

Upgrading replaces the binary only; your machines under `~/.jailmachine`
are untouched. A newer `jm` may default to a newer prebaked guest image,
which only affects machines you create afterwards — there is no in-place
guest upgrade in this release. To move an existing machine to a new guest
image, remove it and create it again:

```bash
jm rm && jm init && jm start
```

If a machine was running while you upgraded, restart it so the new binary
owns the hypervisor and forwarder processes:

```bash
jm stop && jm start
```

## Uninstall completely

```bash
# 1. stop and remove every machine (this also forgets the podman connections)
jm list
jm rm jailmachine          # repeat per machine, or: jm rm <name>

# 2. remove the binary
brew uninstall --cask jailmachine       # Homebrew
# or: rm -f "$(go env GOPATH)/bin/jm" "$(go env GOPATH)/bin/jpodman" "$(go env GOPATH)/bin/jdocker"
# or: rm -f /opt/homebrew/bin/jm /opt/homebrew/bin/jpodman /opt/homebrew/bin/jdocker

# 3. remove any state left behind
rm -rf ~/.jailmachine

# 4. remove any podman connection jm registered but could not clean up
podman system connection list
podman system connection remove jailmachine
podman system connection remove jailmachine-sock
```

`jm rm` already stops the machine, removes both podman connections, drops
the guest's `known_hosts` entry and deletes the machine directory, so steps
3 and 4 are only needed if a machine was removed by hand or a removal was
interrupted. `rm -rf ~/.jailmachine` is otherwise a complete uninstall of
all runtime state — nothing is stored anywhere else.

`brew uninstall --cask jailmachine` removes `jm`, but **leaves the `jpodman`
and `jdocker` symlinks behind**: Homebrew casks have no uninstall hook, so the
symlink removal lives in the cask's zap stanza. Run
`brew uninstall --zap --cask jailmachine` (or `brew zap --cask jailmachine`)
to take it with you, or delete it by hand as in step 2 above.

Neither `qemu` nor `podman` is removed for you; uninstall them separately if
nothing else uses them.

## Gatekeeper and notarisation

**The released binary is not signed or notarised.** The Homebrew cask
handles this by running `xattr -dr com.apple.quarantine` on `jm` after
installing it, so nothing is needed from you on that path.

If you download an archive from the GitHub releases page by hand, or macOS
otherwise refuses to run the binary ("cannot be opened because the developer
cannot be verified"), strip the flag yourself:

```bash
xattr -d com.apple.quarantine /path/to/jm
```

Binaries produced by `go install`, `make build` or `make install` are built
on your own machine and are never quarantined.

## If something is wrong

| Symptom | Do |
|---|---|
| Anything at all | `jm doctor` — it names the failing tool and the fix |
| `no backend for this platform yet` | You are not on macOS/arm64. This release supports Apple Silicon only |
| `qemu-system-aarch64 hvf` fails | The Mac cannot use Hypervisor.framework (nested VM, or an Intel Mac) |
| `gvproxy` not found | `brew install podman` (or reinstall it); jm looks on `PATH` and then in `/opt/homebrew/opt/podman/libexec/podman/gvproxy` |
| `start` hangs or fails | The error names the stage and the log to read; `jm console -f` follows the guest's serial console |
| Provisioning failed | `jm ssh -- cat /var/log/jm-provision.log` |

More detail on each command, its flags and its exit codes is in
[USAGE.md](USAGE.md); a fuller symptom-by-symptom list is in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).
