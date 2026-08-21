# ADR 0008 — Name resolution parity: the host resolver answers for the guest

- Status: accepted (2026-08-21)

## Context

On Linux, containers inherit the machine's view of names: an internal
hostname, a VPN split-horizon record, a hosts-file override and a link-local
`.local` name all work inside a container because they work outside it. Here
the engine lives in a second operating system with its own resolver state, on
a virtual network from which the host's own resolvers may not be reachable.
Users do not experience this as DNS configuration; they think "the URL works
in my browser, so it must work in my container".

## Decision

- **Resolution parity is a required property of a NetworkProvider (ADR 0004),
  alongside addressing and port mapping**: every name the host resolves, the
  guest and its containers resolve to the same answer, at the time of asking.
- **The host is the source of truth, and it is consulted, not copied.** A
  **host resolver** component answers queries through the *host operating
  system's* own resolution API — the path a host application takes — so all
  host policy applies without `jm` modelling any of it: per-domain and
  interface-scoped resolvers, search-list expansion, hosts-file overrides,
  multicast and link-local discovery.
- **The provider routes guest queries to it, and gives the guest exactly one
  nameserver: the provider's.** We deliberately do not copy the host's resolver
  file into the guest: it is a lossy projection of host policy (scoped
  resolvers, hosts entries and discovery are absent from it), it goes stale at
  the next network change, the servers it names may be unreachable from the
  guest, and it would make the guest depend on host topology (ADR 0001).
- **Well-known aliases are answered locally**, never forwarded: the host alias
  (`host.containers.internal`, `host.docker.internal`), the gateway names, and
  the host's own hostname, which must resolve to the host and not to something
  inside the guest. Parity has to hold *in a container*, so the engine gives
  containers the same alias address the guest sees.
- **The search list is host-derived and refreshed.** `jm` reads the host's
  *effective* search domains from the host's resolver configuration and pushes
  them into the guest, re-pushing on change, so joining or leaving a VPN
  converges without a restart; containers already running keep the list they
  were created with, a documented limitation.
- **Failure is propagated, never papered over.** If the host resolver is
  unavailable or errors, that failure is returned verbatim and the guest fails
  exactly when the host would. Falling back to a public resolver is forbidden:
  on a split-horizon network it answers an internal name with a public address,
  and a wrong answer is worse than no answer.
- **Answers are limited to families the provider can route**: a name with
  addresses only in an unroutable family yields an empty answer, never an
  address the guest cannot reach.

## Consequences

- There is no DNS configuration surface: nothing to set, no per-network
  workaround, and VPN behaviour is inherited rather than reimplemented.
- Parity depends on reaching the host OS resolver rather than re-implementing
  one. Anything that substitutes a self-contained resolver loses scoped and
  link-local names while public ones keep working, an invisible regression,
  so `jm doctor` asserts parity against a name only the host can resolve,
  comparing the address and not merely that resolution succeeded. The
  assertion is made of the *running* resolver, over the wire — it reports its
  own resolution path under a reserved name — because neither the build tag
  read in another process nor an alias round trip can see the regression:
  aliases are answered from jm's own table and never reach the host at all.
