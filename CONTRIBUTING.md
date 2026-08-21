# Contributing

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

## Releasing

Releases are cut by `.github/workflows/release.yml` when a `v*` tag is
pushed; it runs goreleaser, which builds `darwin/arm64` (supported) plus
`linux/arm64` and `linux/amd64` (build-only), uploads the archives and
`checksums.txt` to a GitHub release, and pushes a cask to
`gabrielbelli/homebrew-tap`.

```bash
make release-snapshot     # goreleaser check + dry run into dist/
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

> **Before tagging `v<ver>`**, the guest image release `guest-<GuestVersion>`
> (the `image.GuestVersion` that binary embeds) must already be published:
> `jm init` of the new binary fetches it by default and fails otherwise.

### Secrets

| Secret | Used by | Why |
|---|---|---|
| `GITHUB_TOKEN` | `release.yml`, `guest-image.yml` | Automatic; creates releases and uploads assets in this repository |
| `HOMEBREW_TAP_GITHUB_TOKEN` | `release.yml` (goreleaser `homebrew_casks`) | The automatic token cannot push to another repository. Create a **fine-grained personal access token** with *Contents: read and write* on `gabrielbelli/homebrew-tap` only, and add it under *Settings → Secrets and variables → Actions* of `gabrielbelli/jailmachine` |

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
