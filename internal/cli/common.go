package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// store returns the machine store for --state-root.
func store() *machine.Store { return machine.NewStore(StateRoot()) }

// logf prints a "==> <stage>: <detail>" line, like the PoC's log(). It is
// silenced by -q/--quiet and, under --json, moved to stderr so stdout stays
// parseable; data output and errors never go through it.
func logf(w io.Writer, format string, args ...any) {
	if quiet {
		return
	}
	if jsonOut && w == stdout {
		w = stderr
	}
	fmt.Fprintf(w, "==> "+format+"\n", args...)
}

// loadMachine resolves the optional name argument (see resolveDefault for
// what a missing one means) and loads the record, returning a friendlier
// error when it does not exist.
func loadMachine(args []string) (*machine.Machine, error) {
	name, err := resolveName(args)
	if err != nil {
		return nil, err
	}
	m, err := store().Load(name)
	if errors.Is(err, machine.ErrNotFound) {
		return nil, withHint(fmt.Errorf("machine %q does not exist", name), fmt.Sprintf("run 'jm init%s' to create it, or 'jm list'", nameHint(name)))
	}
	return m, err
}

func nameHint(name string) string {
	if name == machine.DefaultName {
		return ""
	}
	return " " + name
}

// backendFor returns the backend recorded in the machine (ADR 0002: a
// machine refuses to start on a different backend).
func backendFor(m *machine.Machine) (backend.Backend, error) {
	return backend.Get(m.Backend)
}

// providerFor returns the network provider recorded in the machine
// (ADR 0004); records from before providers existed get slirp.
func providerFor(m *machine.Machine) (netprov.Provider, error) {
	return netprov.Get(m.NetworkName())
}

// components resolves both the backend and the provider of a machine.
func components(m *machine.Machine) (backend.Backend, netprov.Provider, error) {
	b, err := backendFor(m)
	if err != nil {
		return nil, nil, err
	}
	p, err := providerFor(m)
	if err != nil {
		return nil, nil, err
	}
	return b, p, nil
}

// combineState merges the hypervisor's and the provider's view into one
// machine state (ADR 0005): running only when both run, stopped only when
// neither does, broken when either is broken or they disagree. An
// unsupervised provider (slirp) lives inside the hypervisor and reports
// "running" regardless, so it cannot disagree with a stopped backend.
func combineState(bs, ps backend.State, supervised bool) backend.State {
	switch {
	case bs == backend.Broken || ps == backend.Broken:
		return backend.Broken
	case bs == backend.Running && ps == backend.Running:
		return backend.Running
	case bs == backend.Stopped && ps == backend.Stopped:
		return backend.Stopped
	case bs == backend.Stopped && !supervised:
		return backend.Stopped
	}
	return backend.Broken
}

// currentState computes the runtime state; it is never cached (ADR 0005).
func currentState(m *machine.Machine) (backend.State, error) {
	b, p, err := components(m)
	if err != nil {
		return "", err
	}
	return stateOf(m, b, p)
}

// stateOf is currentState with the components already resolved.
func stateOf(m *machine.Machine, b backend.Backend, p netprov.Provider) (backend.State, error) {
	bs, err := b.State(m)
	if err != nil {
		return "", err
	}
	ps, err := p.State(m)
	if err != nil {
		return "", err
	}
	return combineState(bs, ps, p.Capabilities().Supervised), nil
}

// repairBroken converges a broken machine (ADR 0005) to stopped: the
// hypervisor first, then the provider. When the hypervisor is still
// running (e.g. the provider died under a live guest) and graceful is
// set, the guest is asked to power off rather than having the plug
// pulled; only a dead or stale hypervisor is torn down forcibly.
func repairBroken(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider, graceful bool) error {
	stopForwarder(ctx, m, p)
	bs, err := b.State(m)
	if err != nil {
		return err
	}
	if bs == backend.Running && graceful {
		logf(stdout, "repairing %s: shutting the guest down, then stopping stale networking", m.Name)
		guestPoweroff(ctx, m)
	} else {
		graceful = false
		logf(stdout, "repairing %s: stopping stale hypervisor and network", m.Name)
	}
	if err := b.Stop(ctx, m, graceful); err != nil {
		return err
	}
	return p.Stop(ctx, m)
}

// lock takes the per-machine advisory lock and turns ErrLocked into a
// readable message.
func lock(name string) (func(), error) {
	unlock, err := store().Lock(name)
	if errors.Is(err, machine.ErrLocked) {
		return nil, fmt.Errorf("another jm command is operating on %q; try again shortly", name)
	}
	return unlock, err
}

// sshKey is the absolute path of the machine's private key.
func sshKey(m *machine.Machine) string {
	return store().Path(m.Name, machine.SSHKeyFile)
}

// endpointOf returns where the guest's sshd (and optional API socket) is
// reachable according to the provider; it does not need the provider up.
func endpointOf(m *machine.Machine) (netprov.Endpoint, error) {
	p, err := providerFor(m)
	if err != nil {
		return netprov.Endpoint{}, err
	}
	return p.Endpoint(m)
}

// podman runs the host podman client with the given arguments, capturing
// combined output for error reporting.
func podman(ctx context.Context, args ...string) (string, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return "", errors.New("podman not found on PATH (brew install podman)")
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// podmanConnectionRemove forgets both connection entries for m; a missing
// entry is not an error.
func podmanConnectionRemove(ctx context.Context, m *machine.Machine) {
	_, _ = podman(ctx, "system", "connection", "remove", m.Name)
	_, _ = podman(ctx, "system", "connection", "remove", m.SocketConnectionName())
}

// podmanConnectionAdd registers the SSH connection under the machine's
// name (and makes it the default) and, when the provider proxies the
// guest API to a host socket, a second "<name>-sock" connection for it.
func podmanConnectionAdd(ctx context.Context, m *machine.Machine, ep netprov.Endpoint) error {
	add := func(args ...string) error {
		out, err := podman(ctx, append([]string{"system", "connection", "add"}, args...)...)
		if err != nil {
			if out != "" {
				return fmt.Errorf("podman system connection add: %w: %s", err, out)
			}
			return fmt.Errorf("podman system connection add: %w", err)
		}
		return nil
	}
	if err := add("--identity", sshKey(m), "--default", m.Name, m.PodmanURI()); err != nil {
		return err
	}
	if ep.APISocket != "" {
		if err := add(m.SocketConnectionName(), machine.SocketURI(ep.APISocket)); err != nil {
			return err
		}
	}
	return nil
}

// forgetHostKey drops the guest's entry from ~/.ssh/known_hosts, which
// podman-remote trusts.
func forgetHostKey(m *machine.Machine) {
	if ep, err := endpointOf(m); err == nil {
		_ = sshx.ForgetKnownHost(ep.SSHHost, ep.SSHPort)
		return
	}
	_ = sshx.ForgetKnownHost(machine.SSHHost, m.SSHPort)
}

// requireBinary fails early with an install hint when a host tool is
// missing, like the PoC's need().
func requireBinary(name, brew string) error {
	if _, err := exec.LookPath(name); err != nil {
		return withHint(fmt.Errorf("missing %s on PATH", name), "brew install "+brew)
	}
	return nil
}

// consoleHint names the logs to read when a machine fails to come up. The
// backend knows which files it writes (ADR 0002); the CLI does not.
func consoleHint(m *machine.Machine, b backend.Backend) string {
	return logsHint(b.Logs(m), b.Name())
}

// networkHint is consoleHint for the network provider.
func networkHint(m *machine.Machine, p netprov.Provider) string {
	return logsHint(p.Logs(m), p.Name())
}

func logsHint(logs []string, component string) string {
	if len(logs) == 0 {
		return "no logs available on " + component
	}
	return "see " + strings.Join(logs, " and ")
}

// stdout/stderr indirection for tests.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)
