package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// newForwarderCmd is the hidden foreground entry point of the port
// forwarder (ADR 0004). "jm start" launches it detached; it runs until
// SIGTERM/SIGINT (jm stop) and logs to stdout, which the launcher points at
// forwarder.log.
func newForwarderCmd() *cobra.Command {
	return &cobra.Command{
		Use:    forwarder.Command + " [name]",
		Short:  "Run the port-publishing loop in the foreground (internal)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			p, err := providerFor(m)
			if err != nil {
				return err
			}
			ep, err := p.Endpoint(m)
			if err != nil {
				return err
			}
			logger := log.New(stdout, "forwarder: ", log.LstdFlags)
			logger.Printf("starting for %s (guest %s, publishing on %s, pid %d)",
				m.Name, ep.GuestIP, forwarder.HostIP(m.PublishAddr), os.Getpid())
			defer logger.Printf("stopped")
			return forwarder.Run(cmd.Context(), forwarder.Config{
				Provider:  p,
				Machine:   m,
				GuestIP:   ep.GuestIP,
				HostIP:    m.PublishAddr,
				Engine:    podmanEngine{connection: m.Name},
				Guest:     sshGuest{m: m},
				StatePath: forwarder.StatePath(m.Dir),
				SSHLocal:  net.JoinHostPort(ep.SSHHost, strconv.Itoa(ep.SSHPort)),
				Log:       logger,
			})
		},
	}
}

// podmanEngine implements forwarder.Engine with the host podman client over
// the machine's ssh:// connection.
type podmanEngine struct {
	connection string
}

// psTimeout bounds one "podman ps" over SSH so a wedged connection cannot
// stall the reconciliation loop; inspectTimeout does the same for the
// batched "podman inspect" that resolves container addresses, and
// guestRuleTimeout for the SSH command that loads jm's pf anchor.
const (
	psTimeout        = 30 * time.Second
	inspectTimeout   = 30 * time.Second
	guestRuleTimeout = 30 * time.Second
)

func (e podmanEngine) PS(ctx context.Context) ([]byte, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return nil, errors.New("podman not found on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--connection", e.connection, "ps", "--format", "json").Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Run logs the error; "timed out" names the cause.
			return nil, fmt.Errorf("podman ps: timed out after %s", psTimeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("podman ps: %w: %s", err, ee.Stderr)
		}
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	return out, nil
}

// Inspect batches one "podman inspect" for the containers whose published
// ports need a guest-side redirect (forwarder.Rule); the addresses it
// returns are recomputed on every reconcile, because a restarted container
// gets a new one.
func (e podmanEngine) Inspect(ctx context.Context, ids []string) ([]byte, error) {
	if len(ids) == 0 {
		return []byte("[]"), nil
	}
	bin, err := exec.LookPath("podman")
	if err != nil {
		return nil, errors.New("podman not found on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	args := append([]string{"--connection", e.connection, "inspect", "--type", "container", "--format", "json"}, ids...)
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("podman inspect: timed out after %s", inspectTimeout)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("podman inspect: %w: %s", err, bytes.TrimSpace(ee.Stderr))
		}
		return nil, fmt.Errorf("podman inspect: %w", err)
	}
	return out, nil
}

// sshGuest is the forwarder's control channel into the guest: it loads jm's
// pf anchor over the same SSH connection everything else uses. The whole
// rule set is written on every change, so the anchor is a pure function of
// the desired state and a crash cannot leave a rule behind that the next
// reconcile does not overwrite.
type sshGuest struct{ m *machine.Machine }

func (g sshGuest) ApplyRules(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, guestRuleTimeout)
	defer cancel()
	ep, err := endpointOf(g.m)
	if err != nil {
		return err
	}
	c, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, g.m.SSHUser, sshKey(g.m))
	if err != nil {
		return err
	}
	defer c.Close()
	_, errOut, err := c.RunScript(ctx, "loading jm's redirect rules", forwarder.AnchorScript(text))
	if err != nil {
		if msg := strings.TrimSpace(errOut); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

func (e podmanEngine) Events(ctx context.Context) (io.ReadCloser, error) {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return nil, errors.New("podman not found on PATH")
	}
	cmd := exec.CommandContext(ctx, bin, "--connection", e.connection, "events", "--format", "json", "--filter", "type=container")
	cmd.Stderr = stderr
	// Own process group: podman forks ssh, and killing the group takes the
	// whole tree down with it, on Close and on context cancellation alike.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process) }
	rc, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("podman events: %w", err)
	}
	return &processReader{ReadCloser: rc, cmd: cmd}, nil
}

// killGroup SIGKILLs p's process group, falling back to the process alone.
func killGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return p.Kill()
}

// processReader closes the pipe and reaps the process group on Close.
type processReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (r *processReader) Close() error {
	_ = r.ReadCloser.Close()
	_ = killGroup(r.cmd.Process)
	_ = r.cmd.Wait()
	return nil
}

// forwarderProcess locates m's detached forwarder.
func forwarderProcess(m *machine.Machine) forwarder.Process {
	return forwarder.Process{Dir: m.Dir, Name: m.Name, Root: StateRoot()}
}

// startForwarder is the "forwarder" stage of jm start: idempotently launch
// the detached loop. Providers that give the guest no host-reachable address
// (slirp) cannot publish ports; the stage says so and moves on.
func startForwarder(m *machine.Machine, p netprov.Provider, ep netprov.Endpoint) error {
	if ep.GuestIP == "" {
		logf(stdout, "%s: %s networking cannot publish container ports; skipping", machine.StageForwarder, p.Name())
		return nil
	}
	exe, err := jmBinary()
	if err != nil {
		return machine.NewStageError(machine.StageForwarder, "", fmt.Errorf("locating the jm binary: %w", err))
	}
	pr := forwarderProcess(m)
	if _, ok := pr.Alive(); ok {
		logf(stdout, "%s: port forwarder already running", machine.StageForwarder)
		return nil
	}
	logf(stdout, "%s: starting the port forwarder, publishing on %s (log: %s)",
		machine.StageForwarder, forwarder.HostIP(m.PublishAddr), pr.LogPath())
	if err := pr.Start(exe); err != nil {
		return machine.NewStageError(machine.StageForwarder, "see "+pr.LogPath(), err)
	}
	return nil
}

// stopForwarder terminates the forwarder and, while the provider is still
// up, releases the mappings it owned (best effort). It is called before the
// hypervisor and provider are stopped.
func stopForwarder(ctx context.Context, m *machine.Machine, p netprov.Provider) {
	if m.Dir == "" {
		return
	}
	pr := forwarderProcess(m)
	if err := pr.Stop(ctx); err != nil {
		fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
	}
	if st, err := p.State(m); err != nil || st != backend.Running {
		return
	}
	if err := forwarder.Release(ctx, p, m, forwarder.StatePath(m.Dir)); err != nil && !errors.Is(err, netprov.ErrUnsupported) {
		fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
	}
}

// forwardState reads the forwarder's persisted state for
// inspect/list/ports: the mapping table, its per-mapping errors, and the
// publish address the running forwarder was started with. It never talks to
// the provider, and an unreadable or missing file is an empty state.
func forwardState(m *machine.Machine) *forwarder.State {
	if m.Dir == "" {
		return &forwarder.State{}
	}
	st, err := forwarder.Load(forwarder.StatePath(m.Dir))
	if err != nil {
		return &forwarder.State{}
	}
	return st
}

// forwards is forwardState's mapping table alone.
func forwards(m *machine.Machine) []forwarder.Entry { return forwardState(m).Owned }
