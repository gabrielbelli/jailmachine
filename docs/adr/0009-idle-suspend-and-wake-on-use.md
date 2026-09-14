# ADR 0009 — An idle machine is suspended to its directory and resumed on first use

- Status: accepted (2026-09-14)

## Context

A long-idle machine holds host memory up to its guest's memory size, even with
no workload: file-system caches grow until the host process is as large as the
guest. On this host class, memory handed back through guest cooperation is not
reclaimed; it moves into the host's compressor and the process's accounted
footprint does not fall. Pausing the guest keeps every page. Only ending the
hypervisor process returns the memory, and until now that lost the guest's
state and turned the next use into a cold boot of tens of seconds.

Clients reach the engine through fixed host endpoints that jm hands out (ADR
0001, ADR 0004). Several clients use those endpoints directly and never pass
through jm, so a machine that is not reachable at its endpoints breaks them.

## Decision

1. **A lifecycle state `suspended`.** The guest's complete execution state is
   saved in the machine's directory and no hypervisor or network-provider
   process runs. The state is computed from a transition journal validated
   against processes and file sizes, never cached; read commands never block
   and never resume a machine.

2. **Suspend is an optional backend capability** (ADR 0002). A backend without
   it never suspends, and says why; so does a machine whose hypervisor was
   launched in a way that cannot be saved. The network provider is not asked to
   keep state across a suspend: it is stopped and started again, its dynamic
   mapping table is re-derived by reconciliation (ADR 0004), and the host
   resolver keeps its address across the restart (ADR 0008).

3. **Transitions are journalled and crash-safe.** The journal is written before
   the first change. The saved image is made durable before the journal commits
   it, and the commit comes before the hypervisor is told to exit. On resume the
   journal is removed before the guest executes again, so a saved state is
   never loaded twice. Every mutating command first resolves an interrupted
   transition: an interrupted suspend returns to running when the guest can
   continue, and to stopped otherwise; an interrupted resume is retried. A
   guest that can be continued is never killed, and a guest whose saved state
   is committed is never continued.

4. **The disk is the durable truth; the saved state is a cache of it.** A resume
   uses the hardware description the guest was saved with, pinned to an exact
   hypervisor machine-model version, and never re-reads per-shell settings. A
   resume the hypervisor rejects, or a disk, firmware store or hardware
   description that differs from the saved one, discards the saved state and
   boots from disk with a warning in the same command. Environmental failures (a
   missing program, a busy port, a timeout) keep the saved state. Changes to
   virtual hardware are refused while suspended.

5. **Quiescence.** Before the freeze, host directory shares are detached inside
   the guest without forcing, and file systems are flushed. A share in use
   cancels the suspend. After resume, and before any client is served, the
   shares are re-attached, the guest clock is stepped, and machine settings
   changed while asleep are applied.

6. **Idle policy.** A per-machine supervisor evaluates signals from inside the
   guest over the control channel (running jails and containers, engine client
   sessions other than jm's own, command sessions, an inhibit marker) and from
   the host (jm client invocations, hypervisor CPU use). A sample is idle only
   when every signal is; an unreadable sample is not idle. Time counts only
   while the host is awake. The decision is re-checked under the machine lock
   and again inside the guest immediately before the freeze, and any client
   arriving in between cancels it. Running workloads always keep a machine
   awake; an explicit request can override that, never a share in use. The
   idle period is a per-machine setting on the record; zero disables it. It is
   not read from the environment. Repeated automatic failures turn automatic
   suspend off until the supervisor restarts, and a machine woken soon after
   it was suspended waits longer before the next automatic suspend.

7. **Wake on first use.** Every jm entry point that needs the engine resumes a
   suspended machine, whatever the setting that governs booting a stopped one.
   While a machine is suspended, the supervisor holds the same host endpoints
   clients were given: a connection there starts a resume, is held without
   being read, and is relayed byte for byte to the real endpoint once the
   engine answers. The supervisor never reads, answers or alters engine
   traffic, closes a connection it cannot hand over or has held too long, and
   steps out of the path once the real endpoints are back. It never runs the
   resume itself: a separate one-shot process does, under the machine lock. A
   running machine has no supervisor in its data path.

8. **Stopping and removing.** `stop` on a suspended machine resumes it and shuts
   it down cleanly; a forced stop and `rm` discard the saved state. The
   supervisor is stopped first in both.

## Consequences

- An idle machine costs one small host helper instead of its guest's memory.
- The first request after sleep waits seconds, not tens of seconds; a client
  whose own timeout is shorter fails once.
- While suspended the machine uses host disk up to its memory size, so suspend
  checks free space and declines rather than fill the volume.
- Published container ports do not wake a machine; a machine is only suspended
  with workloads running on explicit request.
- After a host restart, or with the supervisor dead, only jm entry points wake
  a suspended machine; clients that use the endpoints directly fail fast until
  then.
- A hypervisor or host platform update between suspend and resume can make a
  saved state unusable: the guest's processes are lost, its disk is not.
- A machine whose hypervisor was started without support for suspend must be
  restarted once.
- One more supervised helper per machine, stopped first on stop and remove.
- Guest clocks jump forward at wake, as after a long host sleep.
- The saved state holds guest memory, secrets included, in the machine's
  directory until the next resume or discard.

## Alternatives rejected

- Returning memory from a running guest: on this host class the pages are
  compressed, not freed.
- Pausing: keeps all memory.
- A permanent engine-aware proxy in front of every machine: contradicts ADR 0001.
- Waking only through jm's wrappers: breaks the endpoint-based clients that
  `env` advertises.
