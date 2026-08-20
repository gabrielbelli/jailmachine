# ADR 0005 — State lives in one directory per machine; lifecycle is idempotent and crash-tolerant

- Status: accepted (2026-08-20)

## Context

`jm` supervises external processes (hypervisor, network provider, forwarder)
that can outlive or die under it. Users will `Ctrl-C` mid-`start`, reboot the
host, and run two `jm` commands at once.

## Decision

- All state for a machine is under `<state-root>/machines/<name>/`: the
  Machine record, disk, seed, firmware variables, SSH identity, sockets,
  pid files and logs. Nothing elsewhere except the engine-client connection
  entries `jm` registers on behalf of the user (and removes on `rm`).
- The record is the source of *configuration*; the source of *runtime
  state* is the processes themselves (pid + liveness, sockets answering).
  `State()` is always computed, never cached.
- Lifecycle states: `defined → stopped ⇄ running`, with `broken` as a
  diagnosed, recoverable condition (e.g. pid file without process). Every
  command is idempotent: `start` on running skips the boot and re-checks
  the remaining (idempotent) stages with a message, `stop` on stopped is a
  no-op, `rm` always converges to "gone".
- One advisory lock per machine serialises mutating commands; read commands
  (`list`, `inspect`) never block.
- `start` is a staged, resumable sequence — provider up, backend up, SSH
  reachable, ready marker, API connected — each stage reporting progress and
  each failure naming the stage and the log to read. The ready-marker stage
  also reboots the guest once when the kernel on disk differs from the
  running one (the pkgbase image's first-boot base upgrade), so the kernel
  modules podman needs are loaded before the API is connected.

## Consequences

- `rm -rf` of the machine directory is a valid, complete uninstall.
- Multiple named machines fall out naturally; "default machine" is a pointer
  in the state root, not special-cased logic.
- Logs and sockets have fixed, documented paths; `jm doctor`/`inspect` can
  always explain what is running and why something is not.

## Addendum (2026-08-20): one socket may live outside the directory

macOS and the BSDs cap a unix socket path at 104 bytes. When
`<state-root>/machines/<name>/qmp.sock` would exceed that (deep state
roots, long names), the QEMU backend places the socket at a short,
deterministic path under the system temp dir instead (`QMPSocket`). This is
the only file a machine keeps outside its directory. To keep `rm` converging
to "gone", backends expose an optional `Cleaner` hook that `jm rm` calls
before deleting the directory; `rm -rf` alone leaves at most one stale,
harmless socket file in `$TMPDIR`.
