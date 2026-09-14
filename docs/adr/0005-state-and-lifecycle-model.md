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

macOS and the BSDs cap a unix socket path at 104 bytes including the
terminating NUL, so jm allows 103 (`backend.MaxSocketPath`, the figure
`jm doctor` reports). When
`<state-root>/machines/<name>/qmp.sock` would exceed that (deep state
roots, long names), the QEMU backend places the socket at a short,
deterministic path under the system temp dir instead (`QMPSocket`). This is
the only file a machine keeps outside its directory. To keep `rm` converging
to "gone", backends expose an optional `Cleaner` hook that `jm rm` calls
before deleting the directory; `rm -rf` alone leaves at most one stale,
harmless socket file in `$TMPDIR`.

Likewise, `jm set --disk` on a running machine needs the hypervisor told
that the image grew (it reads the size only at boot); backends that can do
so implement the optional `Resizer` hook (QEMU: QMP `block_resize`), and
the guest-side grow verifies the presented size before touching the
partition, so `set` cannot report success while the pool is unchanged.
Backends without `Resizer` require the machine stopped for `--disk`.

## Addendum (2026-09-14): suspended is a lifecycle state

Per **ADR 0009**, lifecycle states become `defined → stopped ⇄ running ⇄
suspended`, with `suspended → stopped` by `stop` (resume, then shut down) or by
a discard (`stop --force`, `rm`, or a saved state the hypervisor rejects);
`broken` is unchanged. `suspended` is computed from a saved-state journal
validated against processes and file sizes, like a pid file, with no hypervisor
round trip; read commands still never block and never resume a machine. While a
transition runs the hypervisor is alive and the machine reads `running`;
commands that need the engine treat a present journal as not ready.

Every mutating command resolves an interrupted transition first, under the lock,
before anything else. The idle supervisor takes the lock only for a transition,
never waits for it, and never holds it while the machine is idle or suspended.
`start` recomputes the state after repairing a broken machine, because a
repaired machine may be suspended. A resume that succeeds up to the guest
running but fails later leaves the machine running, and the next `start`
finishes the remaining stages, as for any interrupted start. `rm -rf` of the
directory remains a complete uninstall: the saved state and the supervisor's
files live in it.
