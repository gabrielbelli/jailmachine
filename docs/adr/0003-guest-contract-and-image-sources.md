# ADR 0003 — A guest contract, with image sources as interchangeable providers

- Status: accepted (2026-08-20)

## Context

The guest can come from a prebuilt release artefact, from an official FreeBSD
image provisioned on first boot, or from a user-supplied image. The host
client must treat all three identically after `init`.

## Decision

Define a **guest contract** — the minimum a guest must satisfy for `jm` to
manage it:

1. Boots from a raw disk with an EFI system partition; root filesystem grows
   to the disk on first boot.
2. Consumes a **first-boot seed** (NoCloud-style data on a secondary disk)
   carrying: SSH authorised key, hostname, and a provisioning script.
3. After provisioning, exposes: `sshd` for the configured user, the container
   engine API on a fixed unix socket path, and a **ready marker** file whose
   presence means "provisioning finished successfully".
4. Provisioning is idempotent and leaves a log at a fixed path.

An **image source** is any provider that yields a disk satisfying (1) and
accepting (2):

| Source | Yields |
|---|---|
| Release artefact | Disk already provisioned; seed only applies keys/hostname → fast first boot |
| Official upstream image | Stock disk; seed carries the full provisioning script |
| Bring-your-own | Path/URL; `jm` verifies (1) and applies the seed, user is responsible for (3)–(4) |

One provisioning script is the single source of truth; the release artefact
is built by running it, so the fast path can never diverge from the slow one.

## Consequences

- `jm start` has one readiness algorithm for every source: wait for SSH, wait
  for the marker, connect the API.
- Image verification (checksums, signatures) is a property of the source,
  surfaced uniformly as "trusted / untrusted" in `inspect`.
- Guest upgrades are "init a new machine from a newer image", not in-place
  mutation; data that must survive lives in volumes the user can export.
