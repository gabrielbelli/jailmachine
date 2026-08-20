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
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// store returns the machine store for --state-root.
func store() *machine.Store { return machine.NewStore(StateRoot()) }

// logf prints a "==> ..." stage line, like the PoC's log().
func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "==> "+format+"\n", args...)
}

// loadMachine resolves the optional name argument and loads its record,
// returning a friendlier error when it does not exist.
func loadMachine(args []string) (*machine.Machine, error) {
	name, err := machine.ResolveName(args)
	if err != nil {
		return nil, err
	}
	m, err := store().Load(name)
	if errors.Is(err, machine.ErrNotFound) {
		return nil, fmt.Errorf("machine %q does not exist (run 'jm init%s')", name, nameHint(name))
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

// currentState computes the runtime state; it is never cached (ADR 0005).
func currentState(m *machine.Machine) (backend.State, error) {
	b, err := backendFor(m)
	if err != nil {
		return "", err
	}
	return b.State(m)
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

// podmanConnectionRemove forgets the connection entry for m; a missing
// entry is not an error.
func podmanConnectionRemove(ctx context.Context, m *machine.Machine) {
	_, _ = podman(ctx, "system", "connection", "remove", m.Name)
}

// podmanConnectionAdd registers (and makes default) the connection entry
// pointing at the guest's podman socket over SSH.
func podmanConnectionAdd(ctx context.Context, m *machine.Machine) error {
	out, err := podman(ctx, "system", "connection", "add", "--identity", sshKey(m), "--default", m.Name, m.PodmanURI())
	if err != nil {
		if out != "" {
			return fmt.Errorf("podman system connection add: %w: %s", err, out)
		}
		return fmt.Errorf("podman system connection add: %w", err)
	}
	return nil
}

// forgetHostKey drops the guest's entry from ~/.ssh/known_hosts, which
// podman-remote trusts.
func forgetHostKey(m *machine.Machine) {
	_ = sshx.ForgetKnownHost(machine.SSHHost, m.SSHPort)
}

// requireBinary fails early with an install hint when a host tool is
// missing, like the PoC's need().
func requireBinary(name, brew string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("missing %s (brew install %s)", name, brew)
	}
	return nil
}

// consoleHint names the logs to read when a machine fails to come up. The
// backend knows which files it writes (ADR 0002); the CLI does not.
func consoleHint(m *machine.Machine, b backend.Backend) string {
	logs := b.Logs(m)
	if len(logs) == 0 {
		return "no logs available on backend " + b.Name()
	}
	return "see " + strings.Join(logs, " and ")
}

// stdout/stderr indirection for tests.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)
