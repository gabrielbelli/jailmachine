# Guest contract (concrete)

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

## Image sources (`internal/image`)

| `--image` | Source | Fetched from | Verified by | `image_trusted` |
|---|---|---|---|---|
| *(default)* / `prebaked` / `prebaked:<ver>` | `Prebaked` | `https://github.com/gabrielbelli/jailmachine/releases/download/guest-<ver>/jailmachine-guest-<ver>-freebsd<rel>-arm64-zfs.raw.zst` | sidecar `<file>.sha256` (`<hash>  <file>`), mandatory | always true |
| `official` / `official:<release>` | `Official` | `download.freebsd.org/releases/VM-IMAGES/<release>/aarch64/Latest/…BASIC-CLOUDINIT-zfs.raw.xz` | `CHECKSUM.SHA256`, mandatory | always true |
| path or https URL to `.raw`, `.raw.xz`, `.raw.zst` | `BYO` | as given | sibling `<file>.sha256` if present | false without a sidecar (warning at `init`, shown by `inspect`) |

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

The build registers a podman connection named `jm-image-build` as the
default while it runs and restores the previous default afterwards.
Publish both files as assets of the GitHub release tagged
`guest-<GuestVersion>`; `jm init` fetches them by default.
