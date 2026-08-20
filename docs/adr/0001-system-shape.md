# ADR 0001 — System shape: thin host client, engine in a guest, one control channel

- Status: accepted (2026-08-20)

## Context

The product promises a Docker-Desktop-like experience for a FreeBSD engine on
hosts that cannot run it natively. Container and jail logic must run inside a
FreeBSD kernel; the host only needs to *reach* it. The host-guest boundary is
the central architectural fact: everything the host tool does is either
lifecycle (make the guest exist and run) or plumbing (make the guest
reachable).

## Decision

Three layers with strict responsibilities:

| Layer | Responsibility | Must not |
|---|---|---|
| **Host client** (`jm`) | Machine lifecycle, image acquisition, networking plumbing, user-facing diagnostics | Contain container/jail semantics; parse OCI; talk to the container runtime directly |
| **Control channel** | A single authenticated channel (SSH) used for provisioning checks, command execution, and tunnelling the engine API | Require a guest-side agent beyond stock `sshd` |
| **Guest engine** | Stock FreeBSD + container runtime + jail manager, configured once at first boot to expose its API on a known socket | Know anything about the host, the hypervisor or the network provider |

Host tools that speak the engine's API (podman CLI, Docker CLI) are
*clients of the guest*, not of `jm`. `jm` only hands them an endpoint.

## Consequences

- No guest agent to maintain: any FreeBSD image that satisfies the guest
  contract (ADR 0003) works.
- Features that need host-guest cooperation (file sharing, port publishing)
  are implemented host-side by observing the guest through the control
  channel, never by pushing code into the guest at runtime.
- The same `jm` binary can drive a remote FreeBSD box: the hypervisor layer
  becomes optional, the control channel does not.
