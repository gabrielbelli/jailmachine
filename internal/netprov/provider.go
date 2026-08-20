// Package netprov defines the network provider abstraction from ADR 0004
// (docs/adr/0004-network-provider.md). A Provider is independent of the
// hypervisor backend: it gives a machine an attachment descriptor for the
// hypervisor, a stable guest address, DNS, a host-side SSH endpoint, an
// optional host-side unix socket proxied to the guest engine API, and a
// port-mapping API that the forwarder (M3) reconciles against.
//
// Providers locate a machine's files through Machine.Dir, like backends,
// and must not know about the state root.
package netprov

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// Endpoint is what a running provider offers the rest of jm.
type Endpoint struct {
	// GuestIP is the guest's stable address as seen from the provider
	// (e.g. 192.168.127.2 for gvproxy). "" when the provider cannot tell
	// (slirp: the guest is unreachable except through hostfwd).
	GuestIP string
	// SSHHost and SSHPort are where the guest's sshd is reachable from the
	// host.
	SSHHost string
	SSHPort int
	// APISocket is a host unix socket proxied to the guest podman API, or
	// "" if the provider offers none (then podman goes over SSH).
	APISocket string
	// DNS is the resolver the guest is handed, "" if unknown.
	DNS string
}

// Mapping is one host<->guest port forward. Local and Remote are
// "host:port" strings; Proto is "tcp" or "udp".
type Mapping struct {
	Proto  string
	Local  string
	Remote string
}

// String renders a mapping the way "jm inspect" lists it.
func (m Mapping) String() string {
	return fmt.Sprintf("%s %s -> %s", m.Proto, m.Local, m.Remote)
}

// Capabilities describe how a provider behaves; the CLI queries them
// instead of special-casing provider names (ADR 0002/0004).
type Capabilities struct {
	// Supervised is true when the provider is a host process of its own
	// that can die independently of the hypervisor (gvproxy). Unsupervised
	// providers live inside the hypervisor (slirp) and always report
	// Running, so they can never disagree with a stopped backend.
	Supervised bool
}

// APIForwarder is an optional Provider interface for providers whose
// Endpoint.APISocket is served by a helper that needs the guest's sshd to
// be up first. "jm start" calls it after the guest is provisioned; Stop
// must tear the helper down as well.
type APIForwarder interface {
	StartAPIForward(ctx context.Context, m *machine.Machine) error
}

// ErrUnsupported is returned by providers that have no port-mapping API
// (slirp): the CLI reports "port publishing needs the gvproxy provider".
var ErrUnsupported = errors.New("netprov: operation not supported by this network provider")

// Provider is implemented by each networking package (netprov/user,
// netprov/gvproxy, later vmnet).
type Provider interface {
	// Name is the identifier stored in the machine record (e.g. "gvproxy").
	Name() string
	// Preflight checks that the host has what the provider needs and
	// returns an error with an install hint otherwise.
	Preflight() error
	// Start brings the provider up for m (before the backend boots, since
	// the hypervisor may connect to it) and returns the attachment the
	// backend should use plus the resulting endpoint. Starting a running
	// provider returns its current attachment and endpoint.
	Start(ctx context.Context, m *machine.Machine) (backend.NetAttachment, Endpoint, error)
	// Stop tears the provider down; it is idempotent.
	Stop(ctx context.Context, m *machine.Machine) error
	// State computes the provider's state from live processes and sockets,
	// never from a cache (ADR 0005).
	State(m *machine.Machine) (backend.State, error)
	// Endpoint returns the endpoint m would have when running. It does not
	// require the provider to be up.
	Endpoint(m *machine.Machine) (Endpoint, error)
	// Expose adds a host->guest port mapping; Unexpose removes one; List
	// returns the current table. Providers without a mapping API return
	// ErrUnsupported.
	Expose(ctx context.Context, m *machine.Machine, mp Mapping) error
	Unexpose(ctx context.Context, m *machine.Machine, mp Mapping) error
	List(ctx context.Context, m *machine.Machine) ([]Mapping, error)
	// Logs lists host-side files worth reading when networking fails, most
	// useful first.
	Logs(m *machine.Machine) []string
	// Capabilities reports how the provider behaves.
	Capabilities() Capabilities
}

// ProviderEnv overrides the host default provider name.
const ProviderEnv = "JM_NETWORK"

// DefaultForHost picks the provider for this host (gvproxy everywhere it
// runs, with an override via $JM_NETWORK, e.g. JM_NETWORK=user for the M1
// slirp behaviour). It returns a name, not a Provider, so callers get the
// registry's "unknown provider" error if nothing matches.
func DefaultForHost() string {
	if v := os.Getenv(ProviderEnv); v != "" {
		return v
	}
	return "gvproxy"
}

var (
	mu       sync.RWMutex
	registry = map[string]Provider{}
)

// Register adds a provider by its Name. Panics on duplicates, like
// database/sql drivers.
func Register(p Provider) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[p.Name()]; dup {
		panic("netprov: duplicate registration of " + p.Name())
	}
	registry[p.Name()] = p
}

// Get returns the provider registered under name.
func Get(name string) (Provider, error) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("netprov: unknown network provider %q (known: %v)", name, names())
	}
	return p, nil
}

// Names lists registered providers, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	return names()
}

func names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
