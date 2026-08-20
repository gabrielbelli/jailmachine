# ADR 0002 — Hypervisor behind a backend interface; machine definition is backend-neutral

- Status: accepted (2026-08-20)

## Context

The first backend choice already changed once (Apple Virtualization.framework
cannot boot FreeBSD; see `docs/tech-choices.md`). Linux and Windows hosts
will need different hypervisors. The thing users care about — "my machine,
4 CPUs, 64 GB, this image" — must outlive any of them.

## Decision

- A **Machine** is a backend-neutral record: name, image reference, resources,
  MAC, SSH identity, network endpoints, creation time. It is the unit of
  state (ADR 0005).
- A **Backend** turns a Machine into a running virtual machine and back:

  ```
  Start(machine) -> running handle     Stop(machine, graceful)
  State(machine) -> stopped|running    Console(machine) -> log stream
  Capabilities() -> {serialConsole, fileSharing, routableNet, ...}
  ```

  It owns hypervisor processes, firmware variables and console logs. It does
  not own networking (ADR 0004) or images (ADR 0003); it consumes a disk path
  and a network attachment descriptor.
- Backend selection is per host OS with an override; a Machine records which
  backend created it and refuses to start on another without an explicit
  migrate step.
- Capabilities are queried, never assumed: the CLI degrades features (e.g.
  "no console on this backend") instead of failing obscurely.

## Consequences

- Adding a host platform means one new package implementing the interface
  and no change to lifecycle, image or network code.
- Backend-specific tunables are namespaced (`backend.<name>.*`) in the
  Machine record so they can be ignored by other backends.
- The interface is intentionally small; GUI/console attach, snapshots and
  suspend are added as optional capability interfaces, not as required methods.
