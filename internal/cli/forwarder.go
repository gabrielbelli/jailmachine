package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
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
// stall the reconciliation loop.
const psTimeout = 30 * time.Second

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

// forwards reads the persisted mapping table for inspect/list/ports; it
// never talks to the provider.
func forwards(m *machine.Machine) []forwarder.Entry {
	if m.Dir == "" {
		return nil
	}
	st, err := forwarder.Load(forwarder.StatePath(m.Dir))
	if err != nil {
		return nil
	}
	return st.Owned
}
