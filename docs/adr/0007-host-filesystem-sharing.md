# ADR 0007 — Host filesystem sharing is a backend capability, mounted at the identity path

- Status: accepted (2026-08-21)

## Context

ADR 0006 put host directory sharing out of scope: it needed a guest-side
filesystem driver the kernel lacked. That constraint has lapsed (see
`docs/tech-choices.md`), and sharing is now the largest gap between "a VM I
have to think about" and "the engine is just here".

The hard part is not the transport, it is *where the files appear*. A user
types `-v ./src:/app` from an arbitrary directory and the engine resolving it
runs in the guest. Mapping host paths to different guest paths would force `jm`
to rewrite volume arguments — to parse engine flags, which ADR 0001 forbids.

## Decision

- **ADR 0002's Capabilities gains `FileSharing`.** Such a backend takes
  **share descriptors** at start: `Share{HostPath, GuestPath, ReadOnly, Tag}`.
  `Tag` is a stable, opaque, length-limited identifier derived from the host
  path: it is how the guest addresses the share, and it lives in the Machine
  record so the guest's mount declarations survive restarts.
- **Identity-path rule**: `GuestPath == HostPath`, canonicalised host-side, so
  `-v /work/src:/app` and `-v /work/src:/work/src` both work from any
  directory with no argument rewriting anywhere.
- **The share set is a machine property reconciled at every start**, not frozen
  at `init`. It defaults to the smallest set of host roots that satisfies the
  identity rule for ordinary work (home tree, temporary directory,
  removable-media root); `jm set` narrows or widens it and `inspect` lists it.
- **Start validates before it launches**: a host path that has vanished is
  dropped with one warning, because a backend may refuse to boot at all over a
  missing share and an unplugged disk must not break `jm start`.
- **Adding shares must not perturb existing virtual hardware.** Firmware boot
  entries reference device addresses and reordering them can render a machine
  permanently unbootable — a one-way door — so backends pin share-device
  addressing and assert it in their tests.
- **Guest contract addition (extends ADR 0003).** A conforming guest (a) has
  the mount points present after provisioning; (b) mounts every offered share
  at its identity path **before the container engine starts**, declaratively,
  not from a script `jm` pushes at runtime; (c) treats an absent or unmountable
  share as a logged non-event, boot completing regardless; (d) force-unmounts
  on shutdown.
- **Semantics are best-effort POSIX, and the gaps are contractual.** Ownership
  changes, explicitly-set timestamps, device and fifo nodes and case
  sensitivity follow the host; shares carry source trees and data, while
  engine-managed volumes stay the faithful, fast path for build output.
- **Absent capability degrades clearly**: `inspect` reports sharing as
  unsupported with a reason, and the CLI refuses a share request up front
  instead of letting a mount fail inside a container.

## Consequences

- Sharing is optional per backend; a better transport later changes
  `tech-choices.md`, not this ADR and not the CLI.
- The home tree is visible to every container by default: a posture decision
  made once, stated in the README and reversible per machine, not assumed.
- `jm doctor` gains a parity check: a file written on the host is visible to a
  container at the same absolute path.
