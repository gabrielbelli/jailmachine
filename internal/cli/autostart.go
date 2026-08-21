package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// Autostart: the wrapper commands ("jpodman", "jdocker", "jm podman",
// "jm docker") boot a stopped machine themselves, so the VM is something
// the user never has to think about — the engine is simply there. It is a
// property of the wrappers only: "jm ssh", "jm ports" and the rest keep
// reporting a stopped machine as stopped.
const (
	// AutostartEnv turns autostart off when set to a false value
	// ("0", "false", "no"), for scripts that want a hard failure instead
	// of a two-second wait for a VM to boot.
	AutostartEnv = "JM_AUTOSTART"
	// NoAutostartEnv is the same switch spelt the other way round
	// (JM_NO_AUTOSTART=1); either form disables autostart.
	NoAutostartEnv = "JM_NO_AUTOSTART"
	// NoAutostartFlag is the per-invocation opt-out, recognised only as
	// the first argument of a wrapper ("jpodman --no-autostart ps") so it
	// cannot be mistaken for an argument of the container command.
	NoAutostartFlag = "--no-autostart"
	// autostartLockWait bounds the wait for another jm command that is
	// already operating on the machine (typically a concurrent wrapper
	// booting the same machine).
	autostartLockWait = 10 * time.Minute
)

// autostartEnabled reports whether the environment allows autostart.
func autostartEnabled() bool {
	switch os.Getenv(AutostartEnv) {
	case "0", "false", "no", "off":
		return false
	}
	switch os.Getenv(NoAutostartEnv) {
	case "", "0", "false", "no", "off":
		return true
	}
	return false
}

// splitAutostartFlag strips a leading --no-autostart from a wrapper's
// arguments, reporting whether it was there.
func splitAutostartFlag(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == NoAutostartFlag {
		return args[1:], true
	}
	return args, false
}

// clientOnly reports whether an invocation is answered by the client alone
// — a help text, the client version, a completion script. Booting a virtual
// machine to print those would be absurd, so autostart sits them out. Note
// that "podman version" and "docker version" are *not* here: they report
// the engine's version too, which is a fact about the machine.
func clientOnly(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "--help", "-h", "help", "--version", "-v", "-V", "completion":
		return true
	}
	return false
}

// ensureRunning brings the machine up if it is not running, printing one
// line to stderr so the user knows why the command is pausing (stdout stays
// the container command's own). It is safe under concurrent invocations:
// the state is checked without the lock (cheap, no ssh), and the start
// itself waits on the per-machine lock rather than failing, so the second
// of two racing wrappers finds the machine running by the time it gets in.
//
// With autostart off, a machine that is not running is an error naming the
// command that would fix it.
func ensureRunning(ctx context.Context, name string, autostart bool) error {
	m, err := store().Load(name)
	if err != nil {
		return err
	}
	st, err := currentState(m)
	if err == nil && st == backend.Running && engineReachable(m) {
		return nil
	}
	if !autostart {
		return withHint(fmt.Errorf("machine %q is %s and autostart is off", name, stateOrUnknown(st, err)),
			"run 'jm start"+nameHint(name)+"'")
	}
	fmt.Fprintf(stderr, "starting jailmachine %q...\n", name)
	ctx, cancel := context.WithTimeout(ctx, autostartLockWait)
	defer cancel()
	return startQuietly(ctx, name)
}

// engineReachable reports whether the machine's engine can be talked to
// now, not merely whether its processes exist. A machine seconds into its
// boot is "running" by ADR 0005 — the hypervisor and the network are up —
// while the engine socket does not exist yet, and a client sent there fails
// with a connection error. The check is a local connect on a unix socket:
// microseconds, so every wrapper invocation can afford it.
func engineReachable(m *machine.Machine) bool {
	ep, err := endpointOf(m)
	if err != nil || ep.APISocket == "" {
		// No socket to test (slirp): the state is all we have to go on.
		return true
	}
	c, err := net.DialTimeout("unix", ep.APISocket, engineDialTimeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// engineDialTimeout bounds that connect; a unix socket either answers at
// once or is not there.
const engineDialTimeout = 500 * time.Millisecond

// startQuietly runs the start stages without the stage lines and progress
// dots: the wrapper has already said what is happening, and everything the
// command prints after this belongs to podman or docker. Errors are
// untouched — a failed boot still reports its stage and log.
func startQuietly(ctx context.Context, name string) error {
	was := quiet
	quiet = true
	defer func() { quiet = was }()
	return startMachine(ctx, []string{name}, startOpts{waitLock: true, skipIfReady: true})
}

// lockMaybeWait takes the per-machine lock, waiting for it when wait is
// set instead of failing with "another jm command is operating on ...".
func lockMaybeWait(ctx context.Context, name string, wait bool) (func(), error) {
	if !wait {
		return lock(name)
	}
	unlock, err := store().LockWait(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("waiting for another jm command operating on %q: %w", name, err)
	}
	return unlock, nil
}

// stateOrUnknown renders a state for an error message, allowing for a
// machine whose state could not be computed at all.
func stateOrUnknown(st backend.State, err error) string {
	if err != nil || st == "" {
		return "not running"
	}
	return string(st)
}

// autostartWord renders the autostart state for "jm inspect".
func autostartWord(on bool) string {
	if on {
		return "on (" + WrapperName + "/" + DockerWrapperName + " start this machine on demand)"
	}
	return "off ($" + AutostartEnv + ")"
}
