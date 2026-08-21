# Guest contract (concrete)

> **Still an MVP — a working demo**, but a wider one than v0.1.0: as well as
> `jm init && jm start`, native FreeBSD and Linux OCI images, published ports
> and bastille jails, the guest now mounts host directories at identical
> paths (ADR 0007), resolves names through the Mac's own resolver (ADR 0008)
> and keeps its clock in step with the host. This page pins the contract the
> guest image satisfies; the image is published as a `guest-<version>` GitHub
> release and verified by `jm init` against its `.sha256` sidecar. The image
> `jm init` fetches by default is named by `image.GuestVersion` in the binary.

ADR 0003 defines the contract abstractly; this page pins the paths and
behaviour `jm` relies on. Everything here is implemented by
`guest/provision.sh` (the single source of truth), `guest/seal.sh` (what
`jm image build` strips) and `internal/image` (the sources).

## Fixed paths in the guest

| What | Path | Written by |
|---|---|---|
| Ready marker | `/var/db/jm-provisioned` | `provision.sh`, last line of the slow path |
| Failure marker | `/var/db/jm-provision-failed` | `provision.sh` EXIT trap on any non-zero exit |
| Provisioning log | `/var/log/jm-provision.log` | `provision.sh` (`exec > … 2>&1`) |
| Engine API socket | `/var/run/podman/podman.sock` | our `podman_service` rc script (`/usr/local/etc/rc.d/podman_service`) |
| SSH | `sshd`, root, `PermitRootLogin prohibit-password`, key from the seed | `provision.sh` |
| Container storage | `zroot/containers` mounted at `/var/db/containers` | `provision.sh` |
| Share mount script | `/usr/local/etc/rc.d/jm_shares` (`jm_shares_enable=YES`) | `provision.sh`, on both paths |
| Share table (guest) | `/var/db/jm/conf/shares.tab`, a read-only 9p share tagged `jmconf` | the backend, at every start |
| Clock resync | `/usr/local/sbin/jm-rtcsync` + `/usr/local/etc/rc.d/jm_rtcsync` (`jm_rtcsync_enable=YES`) | `provision.sh`, on both paths |
| Clock resync log | `/var/log/jm-rtcsync.log` | `jm_rtcsync` via `daemon(8)` |
| Seed mount (transient) | `/media/nuageinit` | `nuageinit` rc script |
| Seed cache | `/var/cache/nuageinit/user_data` (+ `runcmds`) | `nuageinit` |
| First-boot sentinel | `/firstboot` | the image; removed by `rc(8)` at the end of every boot that had it |

`jm start` has one readiness algorithm for every source: wait for sshd,
wait for the ready marker (fail fast on the failure marker), reboot once
if `freebsd-version -k` differs from `-r`, wait for the socket, connect.

## How the seed is applied (nuageinit, FreeBSD 15.1)

Read from `libexec/nuageinit/nuageinit` and `libexec/rc/rc.d/nuageinit*`
on `releng/15.1`:

- `rc.d/nuageinit` (`REQUIRE: mountcritlocal zfs devmatch`, `BEFORE:
  NETWORKING`, **`KEYWORD: firstboot`**) finds a `cidata`-labelled
  iso9660/msdosfs device, mounts it and runs `/usr/libexec/nuageinit
  /media/nuageinit nocloud`.
- For a `#!`-prefixed `user-data` the Lua script only copies it to
  `/var/cache/nuageinit/user_data` and makes it executable.
- `rc.d/nuageinit_user_data_script` (`REQUIRE: local`, also `KEYWORD:
  firstboot`) executes that cached file after `rc.local`, i.e. after
  `sshd`, `pf`, `podman_service` and friends have already been started
  from `rc.conf`.
- **The `instance-id` in `meta-data` is never read or stored.** There is
  no "already ran for this instance" marker in nuageinit at all.
- What gates the run is `rc(8)` itself: scripts with `KEYWORD: firstboot`
  are only included in the boot order while the sentinel `/firstboot`
  exists, and `rc` deletes the sentinel at the end of that boot
  (rebooting if `/firstboot-reboot` was requested).

Consequences:

- A new `instance-id` does **not** make user-data run again; a present
  `/firstboot` does. `internal/seed` therefore keeps `instance-id` = machine
  name (no random suffix needed), and `seal.sh` recreates `/firstboot`.
- `provision.sh` runs exactly once per disk life unless `/firstboot` is
  recreated; the failure marker is therefore terminal for a machine
  (re-`init` it).
- Because the cached copy of the previous machine's `user_data` (with its
  public key) would otherwise ship in the image, `seal.sh` removes
  `/var/cache/nuageinit`.

## Slow path vs fast path

`provision.sh` is one script with an early exit:

1. Always: `hostname` from `JM_HOSTNAME`, `/root/.ssh/authorized_keys`
   from `JM_SSH_PUBKEY`, `sshd_enable`, `PermitRootLogin
   prohibit-password`, `service sshd keygen`, restart sshd.
2. **If `/var/db/jm-provisioned` exists (prebaked image)**: make sure
   `zfs`, `linux`, `pf`, `podman_service` (and `podman`, `bastille`) are
   enabled and started, wait for the podman socket, `exit 0`. Seconds.
3. Otherwise (official image): disable the pkgbase first-boot upgrade's
   reboot, wait for DNS, `pkg install podman-suite bastille`, ZFS dataset,
   Linuxulator, `pf.conf`, `podman_service` rc script, socket, bastille,
   then `touch /var/db/jm-provisioned`. Minutes.

The prebaked image is produced by running path 3 and sealing, so path 2
can never diverge from it.

## Host filesystem sharing (ADR 0007)

The host exports one virtio-9p device per shared directory, plus one more,
tagged `jmconf`, carrying the table that says which mount tag belongs at
which path. `jm_shares` reads that table at boot and mounts every share at
its **identity path** — the same absolute path it has on the host — so
`-v /work/src:/app` and `-v /work/src:/work/src` both resolve in the guest
with nothing rewriting the argument.

- `# REQUIRE: FILESYSTEMS`, `# BEFORE: LOGIN`: the shares are in place
  before `podman_service` (which requires `LOGIN`) starts.
- The table is re-read on every boot, so `jm set --mount/--unmount` needs
  only a restart; nothing is pushed into the guest over SSH.
- The jm-managed block of `/etc/fstab` is regenerated from the table with
  `late,failok`, so the mounts are declared where an administrator looks
  for them. A mountpoint containing whitespace cannot be spelt in fstab and
  is written as a comment; `jm_shares` mounts it regardless.
- **Nothing here can stop the boot.** A share whose device is not attached
  (an unplugged disk, an older `jm`) is logged and skipped; a guest with no
  `p9fs` mounts nothing at all and boots normally. `KEYWORD: shutdown`
  force-unmounts on the way down.
- `seal.sh` stops the service and removes `/var/db/jm` so the builder's
  host tree never reaches a published image.

Semantics are best-effort POSIX. Ownership follows the host user that runs
the hypervisor; `utimes` is a silent no-op and `chown`/`mkfifo` fail. Shares
carry source trees; engine-managed volumes stay the fast path for build
output.

## Name resolution (ADR 0008)

The guest resolves names the way the host does, because the host answers.
`jm start` runs one host-side resolver per machine (`jm _resolver <name>`,
detached, `resolver.pid` / `resolver.log` / `resolver.addr` in the machine
directory) that answers DNS queries through **the host operating system's
own resolution API**: `getaddrinfo(3)` through libSystem, so scoped and
per-domain resolvers (VPN split horizon), `/etc/hosts` and `.local`
multicast names all apply without `jm` modelling any of them.

The host resolver cannot bind port 53 — that needs root — so the guest runs
the one hop that turns "port 53, as libc insists" into the port it did bind:

- `/var/unbound/unbound.conf`: `local_unbound` from the base system as a
  pure forwarder to `<host alias>@<port>`, no cache, no validator, bound to
  `127.0.0.1` and the guest's own address (a reply must come from the
  address the query went to, or a container drops it as a spoof). unbound's
  built-in blackhole zones for `local.`, `254.169.in-addr.arpa.` and the
  RFC 6761 special-use TLDs (`test.`, `invalid.`, `home.arpa.`, `onion.`)
  are disabled with `nodefault`, or a `.test` name in the host's
  `/etc/hosts` would be NXDOMAIN in a container. `localhost.` is left
  alone: its built-in loopback answer must stand.
- `/etc/resolv.conf`: the host's search list and **exactly one** nameserver,
  the guest's own address, so a container's copy of the file works from its
  own network namespace. `resolv_conf="/dev/null"` in `/etc/resolvconf.conf`
  keeps DHCP from putting the provider's nameserver back.
- `/usr/local/etc/containers/containers.conf.d/50-jailmachine-dns.conf`:
  `host_containers_internal_ip`, so the `host.containers.internal` and
  `host.docker.internal` entries the engine writes into every container's
  hosts file name the user's computer rather than the guest.

All three are rewritten only when their contents change, and nothing is
restarted otherwise, so a `jm start` on a running machine does not disturb
name resolution for containers that are already up. `/etc/resolv.conf` is
only taken over once the forwarder answers, so a failure leaves the guest
with the resolution it already had. The port is remembered across restarts
(`resolver.port`), so a rebooted guest resolves before `jm start` reaches it.

A guest that meets this contract but has no working `local_unbound` — a
bring-your-own image, or a transient failure — does not stop `jm start`: it
warns, leaves the guest with the resolution it booted with and carries on,
and `jm doctor`'s `guest resolver <name>` check reports the loss afterwards.
The one hard failure is a guest whose `/etc/resolv.conf` jm already owns and
which now points at a dead resolver: there is nothing to fall back to.

The same applies to the shares: a guest with no `jm_shares` script has the
devices attached and mounts nothing, which is otherwise silent, so `jm start`
warns and `jm doctor` fails its `share parity <name>` check — it writes a
file on the host and asserts the guest sees it at the same absolute path.

## Clock

A virtual machine's timekeeping stops with its host: after the Mac sleeps
the guest wakes minutes or hours behind, and certificates, builds, package
signatures and container ages are all wrong until something corrects it.
The hypervisor's RTC keeps host wall time, so the correction is the guest's
own and needs neither an NTP server nor anything pushed over SSH:

- `/usr/local/sbin/jm-rtcsync`, compiled by `provision.sh` from a C source
  it writes to `/usr/local/etc/jm-rtcsync.c` (the base system's `cc`; an
  image without a compiler logs the fact and goes without). It reads the
  RTC through `/dev/efi` (`EFIIOC_GET_TIME`) every 10 s and steps the clock
  with `settimeofday` past a 2 s difference.
- `/usr/local/etc/rc.d/jm_rtcsync` (`jm_rtcsync_enable=YES`), installed on
  both provisioning paths, so a prebaked image that predates it picks it up
  on the first boot of a new machine. `REQUIRE: FILESYSTEMS`, `BEFORE:
  LOGIN`: the clock is right before the engine and any container start.
- `machdep.disable_rtc_set=1` (`/etc/sysctl.conf`) is **mandatory**:
  without it the kernel writes the guest's own skew back into the RTC and
  the reference is gone.

`jm start` measures the guest clock against the host's once sshd answers
and steps it when they differ by more than five seconds, so a machine that
was already running through a host sleep, or a guest too old to carry the
service, is right before anything runs in it. `jm doctor` reports the skew
and whether the service is running.

## Image sources (`internal/image`)

| `--image` | Source | Fetched from | Verified by | `image_trusted` |
|---|---|---|---|---|
| *(default)* / `prebaked` / `prebaked:<ver>` | `Prebaked` | `https://github.com/gabrielbelli/jailmachine/releases/download/guest-<ver>/jailmachine-guest-<ver>-freebsd<rel>-arm64-zfs.raw.zst` | sidecar `<file>.sha256` (`<hash>  <file>`), mandatory | always true |
| `official` / `official:<release>` | `Official` | `download.freebsd.org/releases/VM-IMAGES/<release>/aarch64/Latest/…BASIC-CLOUDINIT-zfs.raw.xz` | `CHECKSUM.SHA256`, mandatory | always true |
| path or http(s) URL to `.raw` (or `.img`), `.raw.xz`, `.raw.zst` | `BYO` | as given | sibling `<file>.sha256` if present | false without a sidecar (warning at `init`, shown by `inspect`) |

`<ver>` defaults to `image.GuestVersion` (a constant, bumped by hand when a
new guest image is published); `<rel>` is the release without `-RELEASE`
(`15.1`). Decompression writes zero blocks as holes, so the 64 GiB
`disk.raw` stays sparse on the host (APFS only keeps holes larger than a
few tens of MiB, which real images easily are), then the file is truncated
to `--disk`. ZFS grows into the extra space on first boot (`growfs`).

## What `jm image build` does (maintainers)

```bash
make image [RELEASE=15.1-RELEASE]   # = ./jm image build --release $(RELEASE) --out dist
```

1. `jm --state-root dist/.work init --image official:<release> --ssh-port 2229 jm-image-build`
2. `jm --state-root dist/.work start jm-image-build` (slow path, plus the
   kernel reboot if the pkgbase upgrade installed one)
3. `guest/seal.sh` over ssh, which removes/does:
   - `/root/.ssh/authorized_keys`, `/etc/ssh/ssh_host_*`, `hostname` in
     `rc.conf`
   - `/var/log/jm-provision.log`, `/var/db/jm-provision-failed`,
     `/var/log/nuageinit.log`, truncates every file under `/var/log`
   - `/var/cache/nuageinit`, `/root/.history`, `/tmp/*`, `/var/tmp/*`
   - `/etc/hostid`, `/entropy`, `/var/db/entropy/*` (host identity and
     entropy seeds are regenerated on the next machine's first boot)
   - `pkg clean -ay`
   - `touch /firstboot` and `firstboot_pkg_upgrade_enable=NO`
   - `zpool trim -w zroot` so freed blocks become holes in the host file
     (QEMU `discard=unmap`); no zero-fill, as the stock image has no ZFS
     compression and the zeros would land in the host's sparse `disk.raw`
   - leaves `/var/db/jm-provisioned`, all packages, rc.conf services,
     `pf.conf`, the `podman_service` script and `zroot/containers`
4. `jm stop`, then `zstd -T0 -19 --sparse` of `disk.raw` to
   `dist/jailmachine-guest-<GuestVersion>-freebsd<rel>-arm64-zfs.raw.zst`
   and a `.sha256` sidecar in `sha256sum` format.
5. `jm rm jm-image-build`; `dist/.work` is deleted unless `--keep`.

The build registers a podman connection named `jm-image-build` while it runs
and removes it again with `jm rm jm-image-build` at the end. A default
connection you already had is left alone, but podman promotes the first
connection it is ever given, so the build remembers the previous default and
restores it afterwards.
Publish both files as assets of the GitHub release tagged
`guest-<GuestVersion>`; `jm init` fetches them by default.
