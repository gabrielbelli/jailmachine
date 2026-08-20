// Package backend defines the hypervisor abstraction from ADR 0002
// (docs/adr/0002-backend-abstraction.md). A Backend turns a backend-neutral
// machine.Machine into a running VM and back. It owns hypervisor processes,
// firmware variables and console logs; it does not own networking (ADR 0004)
// or images (ADR 0003) — it consumes a disk path and a NetAttachment.
package backend

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// State is the computed (never cached) lifecycle state of a machine.
type State string

const (
	Stopped State = "stopped"
	Running State = "running"
	// Broken is a diagnosed, recoverable condition, e.g. a pid file whose
	// process is gone (ADR 0005).
	Broken State = "broken"
)

// Capabilities are queried, never assumed: the CLI degrades features
// instead of failing obscurely.
type Capabilities struct {
	SerialConsole   bool
	FileSharing     bool
	RoutableNetwork bool
}

// NetAttachment describes how the hypervisor should attach the VM's NIC.
// Kind "user" means slirp/user-mode networking with an SSH hostfwd (M1);
// later providers (gvproxy, vmnet) add their own kinds.
type NetAttachment struct {
	Kind       string
	HostFwdSSH int
	MAC        string
}

// Backend is implemented by each hypervisor package (e.g. backend/qemu).
// Backends locate a machine's files through Machine.Dir, which the store
// fills in; they must not know about the state root.
type Backend interface {
	// Name is the identifier stored in Machine.Backend (e.g. "qemu").
	Name() string
	// Preflight checks that the host has what the backend needs (binaries,
	// firmware) and returns an error with an install hint otherwise.
	Preflight() error
	// Start boots the machine. It returns once the hypervisor is running,
	// not once the guest is reachable.
	Start(ctx context.Context, m *machine.Machine, net NetAttachment) error
	// Stop powers the machine off; graceful asks the guest to shut down
	// first, otherwise the hypervisor is terminated.
	Stop(ctx context.Context, m *machine.Machine, graceful bool) error
	// State computes the current state from live processes and sockets.
	State(m *machine.Machine) (State, error)
	// ConsolePath is the serial console log file, or "" if unsupported.
	ConsolePath(m *machine.Machine) string
	// Logs lists the host-side files worth reading when the machine fails
	// to come up (hypervisor log, console log, ...), most useful first.
	Logs(m *machine.Machine) []string
	// Capabilities reports what this backend can do.
	Capabilities() Capabilities
}

// BackendEnv overrides the host default backend name.
const BackendEnv = "JM_BACKEND"

// DefaultForHost picks the backend for this host OS (ADR 0002: per host OS
// with an override via $JM_BACKEND). It returns a name, not a Backend, so
// callers get the registry's "unknown backend" error if nothing matches.
func DefaultForHost() string {
	if v := os.Getenv(BackendEnv); v != "" {
		return v
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		return "qemu"
	default:
		return "qemu" // the only backend so far; Get reports it if absent
	}
}

var (
	mu       sync.RWMutex
	registry = map[string]Backend{}
)

// Register adds a backend by its Name. Panics on duplicates, like
// database/sql drivers.
func Register(b Backend) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[b.Name()]; dup {
		panic("backend: duplicate registration of " + b.Name())
	}
	registry[b.Name()] = b
}

// Get returns the backend registered under name.
func Get(name string) (Backend, error) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("backend: unknown backend %q (known: %v)", name, names())
	}
	return b, nil
}

// Names lists registered backends, sorted.
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
