# ADR 0004 — Networking is a separate provider; published ports are reconciled, not requested

- Status: accepted (2026-08-20), amended twice (2026-08-21) — see
  [the guest side of a host-bound publish](#amendment-2026-08-21-the-guest-side-of-a-host-bound-publish)
  and [the default link size](#amendment-2026-08-21-the-default-link-size-is-the-jumbo-frame)

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
  address exactly as a bare-metal FreeBSD would. (Amended below for the one
  publish shape where that is not enough.)

## Consequences

- Works with any engine that emits events and any provider with a mapping
  API; no engine patches, no guest agent.
- Crash-safe: the desired state is derived from the guest, so a restarted
  forwarder rebuilds the mapping table instead of losing it.
- Conflicts (host port already taken) are reported per mapping in `inspect`
  rather than failing the container.
- A provider that yields a LAN-routable address can make the forwarder a
  no-op; the CLI surface does not change.

## Amendment (2026-08-21): the guest side of a host-bound publish

`-p 127.0.0.1:8080:80` means opposite things on the two sides of the VM
boundary. Docker reads the address as a **host** bind address the VM never
sees. The engine in the guest reads it as a **guest** bind address: measured
on a real machine, it redirects the guest's loopback only; `-p 0.0.0.0:…`
gets a redirect to the wildcard, which matches no packet; `-p [::1]:…` gets
no redirect at all. Every one of those forms is therefore unreachable from
the host, and no engine flag, `containers.conf` key or network config
rewrites the address.

The original decision left them listed with a reason instead of published,
which is not parity: a container the user asked to confine to their loopback
answered nowhere.

**Amendment.** The forwarder now owns both halves of such a mapping:

- the host leg stays the provider's, bound at the address the user wrote —
  the machine's `--publish-addr` is only the default for a publish that names
  no address;
- the guest leg is a `rdr` rule the forwarder loads into its own pf anchor,
  `rdr/jm`, over the existing SSH control channel, pointing the guest's own
  address at the container's address on the container network.

The anchor is loaded whole on every change, so it stays a pure function of
the desired state — the same reconciliation property the mapping table has.
What the forwarder remembers having loaded is only a fast path, never
evidence: the guest can empty the anchor on its own (a reboot with
restart-policy containers coming back on the same addresses, `service pf
restart`, `pfctl -F nat`) without anything in the desired state changing. So
the anchor is written again whether or not the rules changed whenever that
memory may be stale — on start, after an event-stream reconnect, and every
few timer resyncs — which is the same "re-sync fully on start, on reconnect
and on a timer" the mapping table gets. Container addresses come from a
batched `podman inspect` issued only for the containers that need a rule,
recomputed every reconcile because a restarted container gets a new one.

### Consequences of the amendment

- The guest is no longer *entirely* unaware of the host: jm writes firewall
  rules in it. It is not a new privilege (jm already has root SSH), but it is
  a wider footprint, and a guest whose `pf.conf` lacks the `rdr/*` anchor
  cannot take the rules. No guest-image change was needed; the anchor the
  image already declares is reused.
- Failures of either half are per mapping and retried, never dropped: the
  host port stays bound while the redirect is missing, exactly as it is
  between `podman run` and the next reconcile.
- The guest leg is IPv4 even for an IPv6 host bind; the container network is
  v4 only. Observable behaviour matches docker, the plumbing does not.
- Two containers can publish the same guest port with different host
  addresses. The engine's own redirect wins, and the second mapping reports
  the clash rather than silently pointing at the first container.
- A killed forwarder leaves rules in the anchor until the next one starts,
  which rewrites it; they also die with the guest.

## Amendment (2026-08-21): the default link size is the jumbo frame

The provider's MTU was left at 1500 for docker-like behaviour, but gvproxy
**drops** rather than fragments an oversized datagram, so 1500 was not
docker-like at all: it capped published UDP at 1472 bytes where Linux would
fragment and deliver. The link size is therefore chosen for the ceiling it
buys, not for familiarity.

**Amendment.** The default MTU is **9000**, virtio-net's jumbo frame, which
the guest NIC already advertises (`JUMBO_MTU`) and gvproxy hands out over
DHCP — no guest change was needed. Measured on an Apple Silicon Mac, that
lifts the published-UDP ceiling from 1472 to **8972 bytes** (every size from
1400 to 8972 echoed back byte-exact through a published `-p …/udp` mapping)
at no cost to TCP, which segments to fit: 10 MB in 2.4–2.8 s at 9000 against
2.9–3.8 s at 1500. `$JM_MTU`, read from the environment at `jm start` and
clamped to 576–9000, overrides it; `JM_MTU=1500` restores Docker's exact link
size. `jm doctor` names the knob alongside the limit it reports.

This does not make the link fragment. There is still a hard ceiling and still
no error when a datagram exceeds it — the wall is simply six times further
out, and now movable.
