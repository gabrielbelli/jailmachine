//go:build e2e

// Package e2e holds end-to-end tests (macOS only, -tags e2e) that boot real
// machines with the built ./jm:
//
//   - TestLifecycle: init, start, podman run --os=linux alpine, port
//     publishing, a live disk grow, stop, a warm start and rm.
//   - TestSuspend: idle suspend and wake on use (ADR 0009) on one machine
//     shared by ordered subtests: the idle timer (a command session holds it
//     off, then it suspends), suspend and resume, wake on connect for each
//     client, a client cancelling a suspend, refusals, ports after a wake,
//     SIGKILLs during a suspend and during a wake, a simulated host reboot,
//     an incompatible machine type and a changed disk, and stop and rm from
//     suspended.
//
// Not covered here: the idle-timer cases for a running container, a
// "jpodman events" client and a guest process working in a share (each
// costs another idle period; the probe's counts are unit-tested), and the
// zombie case, which procx rules out by construction (every helper is
// reparented to launchd; see procx.StartDetached and its tests).
//
// Run them with "make build && JM_E2E=1 make e2e". The environment:
//
//	JM_E2E=1          required; without it every test skips
//	JM_E2E_DISK=12    disk size in GiB for each machine (default 12). jm init
//	                  writes far more real disk than the image holds
//	                  (docs/LIMITATIONS.md), so keep it small on a full volume;
//	                  the live grow step grows to this plus 4
//	JM_E2E_IMAGE=...  image for jm init (default official:15.1-RELEASE)
//	JM_E2E_SLOW=1     also run the idle-timer subtest, which takes about
//	                  15 minutes
//
// The machines use SSH ports 2223 (TestLifecycle) and 2224 (TestSuspend) and
// state roots in the test's temporary directory; nothing under ~/.jailmachine
// is touched. Every command runs under a time limit that leaves room before
// go test's -timeout, and each test removes its machine when it ends. A run
// killed from outside (Ctrl-C, or -timeout itself, which skips cleanups)
// leaves the machine running: its processes' command lines name the state
// root (a go test temporary directory, "ps -ax -o pid,command | grep -e
// TestSuspend -e TestLifecycle"); run "jm --state-root <root> rm --force
// e2e-suspend" (or "e2e") on it.
package e2e
