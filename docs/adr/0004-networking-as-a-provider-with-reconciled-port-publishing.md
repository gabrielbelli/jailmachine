# ADR 0004 — Networking is a separate provider; published ports are reconciled, not requested

- Status: accepted (2026-08-20)

## Context

The guest has no routable address on the host by default, the host has no
agent inside the guest, and users expect `-p 8080:80` to "just work" from a
browser. Different hosts offer different networking primitives.

## Decision

- A **NetworkProvider** is a component independent of the backend. It gives
  a Machine: an attachment descriptor for the hypervisor, a stable guest
  address, DNS, a host-side SSH endpoint, an optional host-side unix socket
  proxied to the guest engine API, and a **port-mapping API**
  (`Expose(hostAddr, guestAddr)`, `Unexpose`, `List`).
- **Port publishing is a reconciliation loop**, not an RPC from the engine:
  a host-side forwarder observes the guest's container state through the
  control channel (container events + inspect), computes the desired set of
  host↔guest mappings, and converges the provider's mapping table to it.
  It re-syncs fully on start, on reconnect and on a timer.
- The guest remains unaware of the host: it publishes ports on its own
  address exactly as a bare-metal FreeBSD would.

## Consequences

- Works with any engine that emits events and any provider with a mapping
  API; no engine patches, no guest agent.
- Crash-safe: the desired state is derived from the guest, so a restarted
  forwarder rebuilds the mapping table instead of losing it.
- Conflicts (host port already taken) are reported per mapping in `inspect`
  rather than failing the container.
- A provider that yields a LAN-routable address can make the forwarder a
  no-op; the CLI surface does not change.
