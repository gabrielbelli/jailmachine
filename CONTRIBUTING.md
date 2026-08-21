# Contributing

> **Still an MVP — a working demo**, but a wider one than v0.1.0. As well as
> `jm init && jm start`, native FreeBSD and Linux OCI images, published ports
> and bastille jails, the tree now carries host directory mounts at identical
> paths (ADR 0007), name resolution 1:1 with the host (ADR 0008), autostart on
> demand from `jpodman`/`jdocker`, and a `jdocker` wrapper for the docker CLI.
> Two known gaps are being worked on right now: `-p 127.0.0.1:PORT:PORT`
> publishes nothing on the host, and Linux containers cannot bind UDP sockets.
> There is deliberately **no** autostart-at-login agent — see
> `docs/tech-choices.md` for why.

## Ground rules

- British English in prose (colour, licence, behaviour).
- `gofmt` clean, `go vet` clean, unit tests must not touch the network.
- Architecture lives in `docs/adr/`; add a new ADR rather than editing an
  accepted one. Concrete tool choices go in `docs/tech-choices.md`.
- Runtime state is never in the repo (`~/.jailmachine/` by default).

## Local loop

```bash
make build        # ./jm with version/commit/date stamped from git
make test         # go vet + go test
make lint         # gofmt -l + go vet
JM_E2E=1 make e2e # end-to-end on a Mac with qemu + podman installed
```

`make e2e` cannot run in GitHub Actions: the arm64 macOS runners are
themselves VMs without nested virtualisation, so HVF is unavailable and
QEMU cannot boot the guest. Run it locally before tagging a release.

### Debugging backends

| Variable | Effect |
|---|---|
| `JM_BACKEND=qemu` | Force a backend (needed on Linux, where there is no default yet) |
| `JM_QEMU_ACCEL=tcg` | Pure emulation (`-cpu cortex-a72`) where HVF/KVM is missing; very slow |
| `JM_NETWORK=user` | QEMU slirp instead of gvproxy (no `jm env`, no port publishing) |
| `JM_HOME=/path` | State root (same as `--state-root`) |

## Cutting a release

**CI releases on tag. The local `goreleaser release` is the fallback.**
Pick one; do not do both by habit, though doing both is now survivable
(see *Re-running a tag* below).

### The normal path: push a tag

```bash
make release-snapshot     # goreleaser check + dry run into dist/
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

`.github/workflows/release.yml` picks the tag up, runs the unit tests, then
runs goreleaser: it builds `darwin/arm64` (supported) plus `linux/arm64` and
`linux/amd64` (build-only), uploads the archives and `checksums.txt` to a
GitHub release, and pushes a cask to `gabrielbelli/homebrew-tap`.

> **Before tagging `v<ver>`**, the guest image release `guest-<GuestVersion>`
> (the `image.GuestVersion` that binary embeds) must already be published:
> `jm init` of the new binary fetches it by default and fails otherwise.

You do not have to push a tag to find out whether the workflow works. Every
branch push and pull request runs it in dry-run mode (the `release-dry-run`
job in `ci.yml` calls `release.yml` itself with `dry_run: true`), and
*Actions → release → Run workflow* does the same on demand. A dry run builds,
archives, generates the changelog and makes the same publish decisions, with
`--skip=publish`: nothing is uploaded and no cask is pushed.

### The fallback: release from a Mac

If CI is unavailable, a maintainer can publish the same artefacts by hand:

```bash
export GITHUB_TOKEN=$(gh auth token)
export HOMEBREW_TAP_GITHUB_TOKEN=<fine-grained PAT>   # optional, for the cask
goreleaser release --clean
```

This is a fallback, not a habit: it publishes whatever is in the working
tree, from one machine, with no test gate in front of it.

### Re-running a tag

Re-running the tag in CI after a local `goreleaser release` (or after a
failed run) is fine. `release.replace_existing_artifacts: true` in
`.goreleaser.yaml` makes goreleaser **replace** the assets already attached
to the release instead of POSTing duplicates, which is what used to end the
job with:

```
422 Validation Failed [{Resource:ReleaseAsset Field:name Code:already_exists}]
```

The workflow also logs a notice when it finds a release for the tag already
published, so the log says what happened.

### Secrets

| Secret | Used by | Why |
|---|---|---|
| `GITHUB_TOKEN` | `release.yml`, `guest-image.yml` | Automatic; creates releases and uploads assets in this repository |
| `HOMEBREW_TAP_GITHUB_TOKEN` | `release.yml` (goreleaser `homebrew_casks`) | The automatic token cannot push to another repository. See below. |

#### The Homebrew tap token

`HOMEBREW_TAP_GITHUB_TOKEN` is the only secret that has to be created by
hand. Create a **fine-grained personal access token** with *Contents: read
and write* on `gabrielbelli/homebrew-tap` only, and add it under *Settings
→ Secrets and variables → Actions* of `gabrielbelli/jailmachine`.

**A missing token does not fail the release.** `release.yml` checks for the
secret before starting goreleaser; when it is absent it adds
`--skip=homebrew`, publishes everything else normally, and leaves a notice
and a job summary naming this section. `brew install
gabrielbelli/tap/jailmachine` then keeps serving the previous version until
the cask is updated — by hand, or by re-running the workflow once the secret
exists.

### Version stamping

`internal/version` holds `Version`, `Commit` and `Date`, set with
`-ldflags -X`. goreleaser fills them from the tag; `make build` from
`git describe`; a bare `go build` leaves `dev`/`none`/`unknown`, which is how
`jm version` tells a development build from a release.

### Guest image

The prebaked guest image (ADR 0003 "release artefact") is published as a
GitHub release named `guest-<version>` holding the `.raw.zst` and its
`.sha256` sidecar (SHA256 is the only verification for the MVP; cosign is
post-MVP).

- **Locally (fast, recommended):** on a Mac with HVF,
  `make image RELEASE=15.1-RELEASE` then
  `gh release create guest-<ver> dist/*.zst dist/*.sha256 --title "guest <ver>"`.
- **Testing before publishing:** point the prebaked source at a flat
  directory with `JM_IMAGE_BASEURL` (the file and its `.sha256` sidecar are
  fetched from `$JM_IMAGE_BASEURL/<file name>`):

  ```bash
  (cd dist && python3 -m http.server 8000) &
  JM_IMAGE_BASEURL=http://127.0.0.1:8000 jm --state-root /tmp/jm-test init test
  JM_IMAGE_BASEURL=http://127.0.0.1:8000 jm --state-root /tmp/jm-test start test
  ```

  Note that `jm doctor` inspects the default state root (`~/.jailmachine`
  or `JM_HOME`) unless `--state-root` is given.
- **In CI:** run the `guest-image` workflow (*Actions → guest-image → Run
  workflow*, input `release`). It runs on `ubuntu-latest` under QEMU TCG
  (`JM_QEMU_ACCEL=tcg`, `JM_BACKEND=qemu`), which is an order of magnitude
  slower than HVF — budget 1–2 hours; the job times out at 180 minutes.

## Commit messages

Conventional-ish: `feat:`, `fix:`, `docs:`, `ci:`, `chore:`, `test:` or the
milestone prefix (`M6: ...`). goreleaser groups the changelog by these
prefixes and drops `test:`, `chore:` and `ci:` entries.
