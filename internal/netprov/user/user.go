// Package user is the M1 network provider: QEMU's built-in slirp
// (user-mode) networking with a single SSH hostfwd. There is no process to
// supervise, no guest address reachable from the host and no port-mapping
// API; it exists so the M1 behaviour stays selectable with JM_NETWORK=user.
package user

import (
	"context"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// Name is the identifier stored in the machine record.
const Name = "user"

// SlirpDNS is the resolver slirp hands the guest.
const SlirpDNS = "10.0.2.3"

// Provider implements netprov.Provider with slirp.
type Provider struct{}

func init() { netprov.Register(Provider{}) }

// Name implements netprov.Provider.
func (Provider) Name() string { return Name }

// Preflight implements netprov.Provider: slirp is built into QEMU.
func (Provider) Preflight() error { return nil }

// Start implements netprov.Provider: nothing to launch; the attachment
// tells the backend to build a hostfwd for SSH.
func (p Provider) Start(_ context.Context, m *machine.Machine) (backend.NetAttachment, netprov.Endpoint, error) {
	ep, _ := p.Endpoint(m)
	return backend.NetAttachment{
		Kind:        backend.KindUser,
		HostFwdAddr: machine.SSHHost,
		HostFwdSSH:  m.SSHPort,
		MAC:         m.MAC,
	}, ep, nil
}

// Stop implements netprov.Provider: nothing to tear down.
func (Provider) Stop(context.Context, *machine.Machine) error { return nil }

// State implements netprov.Provider. Slirp lives inside the hypervisor, so
// the provider itself is always "running" as far as jm is concerned.
func (Provider) State(*machine.Machine) (backend.State, error) { return backend.Running, nil }

// Endpoint implements netprov.Provider. The guest has no address the host
// can reach; only the hostfwd for SSH exists.
func (Provider) Endpoint(m *machine.Machine) (netprov.Endpoint, error) {
	return netprov.Endpoint{SSHHost: machine.SSHHost, SSHPort: m.SSHPort, DNS: SlirpDNS}, nil
}

// Expose implements netprov.Provider: slirp hostfwd cannot be changed at
// run time from here.
func (Provider) Expose(context.Context, *machine.Machine, netprov.Mapping) error {
	return netprov.ErrUnsupported
}

// Unexpose implements netprov.Provider.
func (Provider) Unexpose(context.Context, *machine.Machine, netprov.Mapping) error {
	return netprov.ErrUnsupported
}

// List implements netprov.Provider.
func (Provider) List(context.Context, *machine.Machine) ([]netprov.Mapping, error) {
	return nil, netprov.ErrUnsupported
}

// Logs implements netprov.Provider: slirp writes nothing of its own.
func (Provider) Logs(*machine.Machine) []string { return nil }

// Capabilities implements netprov.Provider: slirp is not a process of its
// own, so it cannot die under the hypervisor.
func (Provider) Capabilities() netprov.Capabilities { return netprov.Capabilities{} }
