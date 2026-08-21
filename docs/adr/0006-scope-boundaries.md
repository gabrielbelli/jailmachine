# ADR 0006 — Scope boundaries for the MVP

- Status: accepted (2026-08-20)

## In scope

- One host platform (the author's), one backend, one network provider —
  each behind the interfaces in ADR 0002/0004.
- Lifecycle of multiple named machines (ADR 0005); image sources per ADR 0003.
- Engine reachability for existing clients: a connection entry for the
  native client, an environment export for socket-based clients.
- Reconciled port publishing (ADR 0004).
- Packaging and a single-command install.

## Out of scope (deliberately)

- Jail management from the host. Jails are a second product surface with its
  own UX; for now they are reached through the control channel (`jm ssh`).
- Host directory sharing. Needs either a guest-side filesystem driver the
  kernel lacks, or a network filesystem with its own lifecycle. Design
  separately once the control channel and state model are stable.
- Additional backends/providers, GUI, snapshots, suspend.
- In-place guest upgrades (ADR 0003: re-init instead).

## Rule

New work enters scope only if it fits an existing interface without widening
it, or comes with a new ADR that widens the interface deliberately.

## Addendum (2026-08-21): host directory sharing is in scope

Host directory sharing moves from "out of scope" to in scope, per **ADR
0007**: the guest-side filesystem driver it was waiting for exists, so it
fits the rule above by widening ADR 0002's Capabilities (a `FileSharing`
capability with share descriptors) and ADR 0003's guest contract (shares
mounted at the identity path before the engine starts), rather than adding a
network filesystem with its own lifecycle. Name resolution parity (**ADR
0008**) is likewise in scope as a property of the existing NetworkProvider.
Both are optional per backend/provider and degrade with a stated reason.
