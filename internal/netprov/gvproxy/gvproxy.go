// Package gvproxy implements netprov.Provider with gvisor-tap-vsock's
// gvproxy, the userspace network stack podman machine uses. gvproxy serves
// the VM's NIC on a unix stream socket (QEMU connects to it), hands the
// guest 192.168.127.2 by DHCP, forwards a host port to guest sshd, proxies
// a host unix socket to the guest podman socket over SSH, and takes
// host->guest port mappings over an HTTP control API.
//
// gvproxy must be up before QEMU starts and must outlive it, so it runs
// detached (own session) with a pid file, log file and the same
// pid-plus-argv liveness rule as the qemu backend.
//
// gvproxy's own -forward-sock is deliberately not used: gvproxy 0.8.x
// exits altogether when guest sshd does not answer within its internal
// timeout, which a FreeBSD first boot routinely exceeds, taking the VM's
// network down with it. The host podman.sock is served instead by a
// detached "ssh -N -L" helper (forward.go) started once the guest is
// provisioned (netprov.APIForwarder).
package gvproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// Name is the identifier stored in the machine record.
const Name = "gvproxy"

// Binary is the executable name looked up on PATH.
const Binary = "gvproxy"

// FallbackBinary is where the Homebrew podman formula ships gvproxy.
const FallbackBinary = "/opt/homebrew/opt/podman/libexec/podman/gvproxy"

// BinaryEnv points at a gvproxy binary explicitly.
const BinaryEnv = "JM_GVPROXY"

// Addresses gvproxy's virtual network uses (fixed in gvproxy).
const (
	GuestIP = "192.168.127.2"
	Gateway = "192.168.127.1"
	// HostAlias is gvproxy's last usable subnet address, which its TCP and
	// UDP forwarders translate to the host's 127.0.0.1 port for port. It
	// is how the guest reaches a host service bound to the loopback, the
	// host resolver of ADR 0008 included.
	HostAlias = "192.168.127.254"
	// DefaultMTU is the guest link size. gvproxy drops rather than
	// fragments a datagram larger than the MTU, so a 1500-byte link caps
	// published UDP at 1472 bytes, where Linux would fragment and deliver.
	// The guest NIC advertises JUMBO_MTU and gvproxy hands the value over
	// DHCP, so we take the jumbo frame instead: measured on an Apple
	// Silicon Mac, that lifts the UDP ceiling from 1472 to 8972 bytes with
	// no cost to TCP throughput (10 MB in 2.4-2.8 s at 9000 against
	// 2.9-3.8 s at 1500). $JM_MTU overrides it, e.g. JM_MTU=1500 to match
	// Docker exactly.
	DefaultMTU = MaxMTU
	// MaxMTU is the largest link size verified end to end on this stack:
	// the guest applied 16384 and single datagrams of 16356 bytes came
	// through, 16357 did not. The ceiling tracks the MTU exactly, so it is
	// the link that limits a datagram, not the guest or the container.
	MaxMTU = 16384
)

// MTU is the link size gvproxy and the guest agree on, overridable with
// $JM_MTU (clamped to [576, MaxMTU]).
func MTU() int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("JM_MTU")))
	if err != nil || v < 576 {
		return DefaultMTU
	}
	if v > MaxMTU {
		return MaxMTU
	}
	return v
}

// Files this provider keeps in the machine directory. Sockets go through
// backend.SocketPath, so they may fall back to the temp dir (see Cleanup).
const (
	NetSockFile    = "net.sock"    // QEMU stream netdev endpoint
	APISockFile    = "api.sock"    // gvproxy HTTP control API
	PodmanSockFile = "podman.sock" // host side of the proxied guest podman socket
	PIDFile        = "gvproxy.pid"
	LogFile        = "gvproxy.log"
	ForwardPIDFile = "forward.pid" // ssh -L helper serving podman.sock
	ForwardLogFile = "forward.log"
)

// Timeouts.
const (
	startTimeout = 10 * time.Second
	stopTimeout  = 10 * time.Second
	pollInterval = 100 * time.Millisecond
)

// ErrNoDir is returned when a Machine was not loaded through the store and
// so has no directory.
var ErrNoDir = errors.New("gvproxy: machine record has no directory (load it through the machine store)")

// Paths are the host files one gvproxy instance uses. Args is a pure
// function of a Machine and Paths so it can be unit-tested.
type Paths struct {
	Net    string // net.sock
	API    string // api.sock
	Podman string // podman.sock
	Key    string // ssh private key for -forward-identity
	PID    string // gvproxy.pid
	Log    string // gvproxy.log
	FwdPID string // forward.pid
	FwdLog string // forward.log
}

// PathsFor returns the paths for a machine directory.
func PathsFor(dir string) Paths {
	return Paths{
		Net:    backend.SocketPath(dir, NetSockFile),
		API:    backend.SocketPath(dir, APISockFile),
		Podman: backend.SocketPath(dir, PodmanSockFile),
		Key:    filepath.Join(dir, machine.SSHKeyFile),
		PID:    filepath.Join(dir, PIDFile),
		Log:    filepath.Join(dir, LogFile),
		FwdPID: filepath.Join(dir, ForwardPIDFile),
		FwdLog: filepath.Join(dir, ForwardLogFile),
	}
}

// Sockets lists the unix sockets, the ones that may live out of tree.
func (p Paths) Sockets() []string { return []string{p.Net, p.API, p.Podman} }

// Args builds the gvproxy argument vector.
func Args(m *machine.Machine, p Paths) []string {
	return []string{
		"-listen-qemu", "unix://" + p.Net,
		"-listen", "unix://" + p.API,
		"-ssh-port", strconv.Itoa(m.SSHPort),
		"-pid-file", p.PID,
		"-log-file", p.Log,
		"-mtu", strconv.Itoa(MTU()),
	}
}

// LookupBinary finds gvproxy: $JM_GVPROXY, then PATH, then the Homebrew
// podman location.
func LookupBinary() (string, error) {
	if v := os.Getenv(BinaryEnv); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("gvproxy: $%s=%s: %w", BinaryEnv, v, err)
		}
		return v, nil
	}
	if bin, err := exec.LookPath(Binary); err == nil {
		return bin, nil
	}
	if _, err := os.Stat(FallbackBinary); err == nil {
		return FallbackBinary, nil
	}
	return "", fmt.Errorf("gvproxy: not found on PATH nor at %s (brew install podman, or set $%s)", FallbackBinary, BinaryEnv)
}

// Provider implements netprov.Provider with gvproxy.
type Provider struct{}

func init() { netprov.Register(Provider{}) }

// Name implements netprov.Provider.
func (Provider) Name() string { return Name }

// Preflight implements netprov.Provider.
func (Provider) Preflight() error {
	_, err := LookupBinary()
	return err
}

// Logs implements netprov.Provider.
func (Provider) Logs(m *machine.Machine) []string {
	if m.Dir == "" {
		return nil
	}
	p := PathsFor(m.Dir)
	return []string{p.Log, p.FwdLog}
}

// Capabilities implements netprov.Provider: gvproxy is a host process that
// can die independently of the hypervisor.
func (Provider) Capabilities() netprov.Capabilities {
	return netprov.Capabilities{Supervised: true, MTU: MTU()}
}

// Endpoint implements netprov.Provider.
func (Provider) Endpoint(m *machine.Machine) (netprov.Endpoint, error) {
	if m.Dir == "" {
		return netprov.Endpoint{}, ErrNoDir
	}
	return netprov.Endpoint{
		GuestIP:   GuestIP,
		SSHHost:   machine.SSHHost,
		SSHPort:   m.SSHPort,
		APISocket: PathsFor(m.Dir).Podman,
		DNS:       GuestIP,
		Gateway:   Gateway,
		HostAlias: HostAlias,
	}, nil
}

func attachment(m *machine.Machine, p Paths) backend.NetAttachment {
	return backend.NetAttachment{Kind: backend.KindStream, SocketPath: p.Net, MAC: m.MAC}
}

// State implements netprov.Provider: computed from the pid file and the
// process behind it, never cached. A live pid whose argv does not mention
// our api.sock (recycled pid, another machine's gvproxy) is Broken.
func (Provider) State(m *machine.Machine) (backend.State, error) {
	if m.Dir == "" {
		return "", ErrNoDir
	}
	p := PathsFor(m.Dir)
	return stateFromPIDFile(p.PID, func(pid int) bool { return isOurs(pid, p.API) })
}

// stateFromPIDFile is the pure core of State, shared with tests.
func stateFromPIDFile(pidFile string, ours func(pid int) bool) (backend.State, error) {
	pid, err := readPID(pidFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return backend.Stopped, nil
	case err != nil:
		return backend.Broken, nil
	}
	if ours(pid) {
		return backend.Running, nil
	}
	return backend.Broken, nil
}

// Repair removes stale runtime files left behind by a dead gvproxy and
// stops the socket forwarder, which is useless without gvproxy.
func (Provider) Repair(m *machine.Machine) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	p := PathsFor(m.Dir)
	return errors.Join(stopForward(context.Background(), p), removeAll(append(p.Sockets(), p.PID)...))
}

// Start implements netprov.Provider: launches gvproxy detached and waits
// until both the QEMU socket and the API socket exist.
func (pr Provider) Start(ctx context.Context, m *machine.Machine) (backend.NetAttachment, netprov.Endpoint, error) {
	ep, err := pr.Endpoint(m)
	if err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, err
	}
	p := PathsFor(m.Dir)
	st, err := pr.State(m)
	if err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, err
	}
	switch st {
	case backend.Running:
		return attachment(m, p), ep, nil
	case backend.Broken:
		if err := pr.Repair(m); err != nil {
			return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: repairing stale state: %w", err)
		}
	}
	bin, err := LookupBinary()
	if err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, err
	}
	if _, err := os.Stat(p.Key); err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: ssh identity missing (run 'jm init'): %w", err)
	}
	for _, s := range p.Sockets() {
		if len(s) > backend.MaxSocketPath {
			return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: socket path %q is %d bytes; unix sockets are limited to %d (use a shorter --state-root or $TMPDIR)", s, len(s), backend.MaxSocketPath)
		}
	}
	// gvproxy refuses to start over stale sockets.
	if err := removeAll(p.Sockets()...); err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: removing stale sockets: %w", err)
	}

	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: opening %s: %w", p.Log, err)
	}
	defer logf.Close()
	// Not CommandContext: gvproxy must outlive this jm invocation.
	cmd := exec.Command(bin, Args(m, p)...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: failed to start: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap in the background so an early exit does not leave a zombie; we
	// never Wait on it synchronously.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	if err := waitSockets(ctx, exited, p.Net, p.API); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = removeAll(append(p.Sockets(), p.PID)...)
		return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: %w: %s", err, tailOf(p.Log))
	}
	// gvproxy writes -pid-file itself; make sure State agrees before we
	// hand the attachment to the backend.
	if _, err := readPID(p.PID); err != nil {
		if werr := os.WriteFile(p.PID, []byte(strconv.Itoa(pid)+"\n"), 0o600); werr != nil {
			return backend.NetAttachment{}, netprov.Endpoint{}, fmt.Errorf("gvproxy: writing %s: %w", p.PID, werr)
		}
	}
	return attachment(m, p), ep, nil
}

// waitSockets polls until every socket exists, the process exits, the
// timeout lapses or ctx is cancelled.
func waitSockets(ctx context.Context, exited <-chan error, socks ...string) error {
	deadline := time.Now().Add(startTimeout)
	for {
		missing := ""
		for _, s := range socks {
			if _, err := os.Stat(s); err != nil {
				missing = s
				break
			}
		}
		if missing == "" {
			return nil
		}
		select {
		case err := <-exited:
			return fmt.Errorf("exited before creating %s (%v)", filepath.Base(missing), err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", filepath.Base(missing))
		}
	}
}

// Stop implements netprov.Provider: SIGTERM, wait, SIGKILL, then remove
// sockets and pid file. A broken provider is repaired; a stopped one is a
// no-op.
func (pr Provider) Stop(ctx context.Context, m *machine.Machine) error {
	st, err := pr.State(m)
	if err != nil {
		return err
	}
	if st != backend.Running {
		return pr.Repair(m)
	}
	p := PathsFor(m.Dir)
	pid, err := readPID(p.PID)
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && processAlive(pid) {
		return fmt.Errorf("gvproxy: SIGTERM pid %d: %w", pid, err)
	}
	if !waitExit(ctx, pid, stopTimeout) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		if !waitExit(ctx, pid, stopTimeout) {
			return fmt.Errorf("gvproxy: pid %d did not exit after SIGKILL", pid)
		}
	}
	return pr.Repair(m)
}

// StartAPIForward implements netprov.APIForwarder: serve podman.sock on
// the host through a detached ssh forward to the guest podman socket. It
// needs gvproxy up and guest sshd answering; it is idempotent.
func (pr Provider) StartAPIForward(ctx context.Context, m *machine.Machine) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	st, err := pr.State(m)
	if err != nil {
		return err
	}
	if st != backend.Running {
		return errors.New("gvproxy: not running; cannot forward the podman socket")
	}
	return startForward(ctx, m, PathsFor(m.Dir))
}

// Cleanup implements backend.Cleaner for the sockets backend.SocketPath may
// have placed outside the machine directory.
func (Provider) Cleanup(m *machine.Machine) error {
	if m.Dir == "" {
		return ErrNoDir
	}
	var out []string
	for _, s := range PathsFor(m.Dir).Sockets() {
		if !backend.InTree(m.Dir, s) {
			out = append(out, s)
		}
	}
	return removeAll(out...)
}

// Expose implements netprov.Provider.
func (Provider) Expose(ctx context.Context, m *machine.Machine, mp netprov.Mapping) error {
	c, err := clientFor(m)
	if err != nil {
		return err
	}
	return c.Expose(ctx, mp)
}

// Unexpose implements netprov.Provider.
func (Provider) Unexpose(ctx context.Context, m *machine.Machine, mp netprov.Mapping) error {
	c, err := clientFor(m)
	if err != nil {
		return err
	}
	return c.Unexpose(ctx, mp)
}

// List implements netprov.Provider.
func (Provider) List(ctx context.Context, m *machine.Machine) ([]netprov.Mapping, error) {
	c, err := clientFor(m)
	if err != nil {
		return nil, err
	}
	return c.List(ctx)
}

func clientFor(m *machine.Machine) (*Client, error) {
	if m.Dir == "" {
		return nil, ErrNoDir
	}
	return NewClient(PathsFor(m.Dir).API), nil
}

func removeAll(paths ...string) error {
	var errs []error
	for _, f := range paths {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// tailOf returns the trimmed tail of a log file, or a hint when empty.
func tailOf(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return "(no output in " + path + ")"
	}
	const max = 4096
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return strings.TrimSpace(string(data))
}
