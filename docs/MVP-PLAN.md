# jailmachine MVP plan

Goal: `brew install gabrielbelli/tap/jailmachine && jm init && jm start &&
podman run -p 8080:80 docker.io/busybox httpd -f -p 80` works on an Apple
Silicon Mac, with both Linux and native FreeBSD images, in under 3 minutes
from a cold start. (`docker.io/nginx` works too, with `accept_mutex on;` in
its `events` block — FreeBSD's `linux_epoll` has no `EPOLLEXCLUSIVE`; see
`docs/USAGE.md` for the verified Docker Hub matrix.)

Architecture: `docs/adr/`. Tools: `docs/tech-choices.md`. Proof of concept: `bin/jm` (shell, works today).

## Repo layout (target)

```
cmd/jm/                 main.go (cobra root)
internal/cli/           one file per subcommand
internal/machine/       Machine struct, state dir, machine.json, lifecycle
internal/backend/       Backend interface
internal/backend/qemu/  QEMU+HVF (macOS) — later +KVM (Linux)
internal/net/gvproxy/   gvproxy process + API client (expose/unexpose)
internal/forwarder/     podman events -> gvproxy port publishing
internal/image/         sources: prebaked (releases), official (download.freebsd.org), byo; verify
internal/seed/          NoCloud ISO builder (pure Go iso9660, no hdiutil)
internal/sshx/          ssh client helpers (wait, run, tunnel)
guest/provision.sh      single provisioning script (embedded)
guest/rc.d/podman_service
docs/adr/               decisions
.github/workflows/      ci (lint, unit, macOS e2e), release (goreleaser), image-build
```

## Milestones

| # | Milestone | Done when | Est. |
|---|---|---|---|
| M0 | Commit PoC | `bin/jm` + docs on `main` | 10 min |
| M1 | Go skeleton | `go build ./cmd/jm`; `jm init/start/stop/ssh/rm` reach PoC parity using QEMU slirp; `machine.json` v1; unit tests for state/image/seed | 2–3 days |
| M2 | gvproxy networking | VM gets 192.168.127.2; SSH via gvproxy `-ssh-port`; host `podman.sock` via a detached `ssh -N -L` forward started after provisioning (gvproxy’s `-forward-sock` exits when guest sshd is slow); `jm env` | 1–2 days |
| M3 | Port publishing | `podman run -p 8080:80 busybox httpd` reachable on host; forwarder survives restarts; `jm inspect` lists forwards. Done as `internal/forwarder`: detached `jm _forwarder` reconciles `podman ps` against gvproxy’s expose API on events/30 s/reconnect; owned set in `forwards.json`; `jm ports`, `PORTS` column in `list`, `forwarder` stage in `start` | 1–2 days |
| M4 | Named machines + UX | `jm list`, `jm set`, `jm console`, progress bars, error messages with fixes | 1–2 days |
| M5 | Prebaked image pipeline | GitHub Actions builds+signs image; `jm init` defaults to it; `--image official` and BYO paths tested | 2–3 days |
| M6 | Release | goreleaser, Homebrew tap, README quickstart, `jm doctor` (checks qemu/gvproxy/podman versions) | 1 day |
| M7 | Docker parity (post-MVP, `docker-parity`) | Host directory shares at identical paths (ADR 0007), name resolution 1:1 with the host (ADR 0008), autostart on demand from `jpodman`/`jdocker`, the `jdocker` wrapper, guest clock resync, and published ports on `0.0.0.0` with `--publish-addr`. Remaining: `-p 127.0.0.1:PORT:PORT` publishes nothing, and Linux containers cannot bind UDP sockets | in progress |

Roughly **two weeks** of focused work; M1–M3 are the critical path.

## M1 detail (start here)

1. `go mod init github.com/gabrielbelli/jailmachine`; cobra root with
   `--machine-dir` (default `~/.jailmachine`), `--json` for `list/inspect`.
2. `internal/machine`: `Machine{Name, Image, CPUs, MemoryMiB, DiskGiB, MAC, SSHPort, Created}`
   persisted as `~/.jailmachine/machines/<name>/machine.json`; lock file per machine.
3. `internal/backend`: `type Backend interface { Start(ctx, *Machine) error; Stop(ctx, *Machine, graceful bool) error; State(*Machine) (State, error) }`.
   QEMU impl: build argv from PoC, `-daemonize -pidfile`, QMP `system_powerdown`.
4. `internal/image`: download with resume + SHA256 from `CHECKSUM.SHA256`;
   `xz` decompression in-process (`github.com/ulikunitz/xz`); sparse truncate.
5. `internal/seed`: NoCloud ISO in Go (`github.com/kdomanski/iso9660`), label `cidata`.
6. `guest/provision.sh`: PoC script verbatim, parametrised by env (`JM_SSH_PUBKEY`).
7. e2e test (macOS only, `-tags e2e`): init → start → `podman run --os=linux alpine echo hi` → stop → rm.

## Open questions (ask Gabriel when reached)

- Module path / GitHub org for the tap (`gabrielbelli/homebrew-tap`?).
- Image signing: cosign keyless (GitHub OIDC) vs plain SHA256 for MVP.
- Guest user: keep `root` over SSH, or add unprivileged `jm` user + sudo?
- Default resources: 4 vCPU / 4 GiB / 64 GiB, or derive from host (½ cores, ¼ RAM)?
