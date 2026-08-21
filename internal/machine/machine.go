// Package machine holds the backend-neutral Machine record and its on-disk
// store (ADR 0002, ADR 0005). All state for a machine lives under
// <state-root>/machines/<name>/.
package machine

import "time"

// Version is the current machine.json schema version.
const Version = 1

// Fixed filenames inside a machine directory (relative to Dir(name)).
const (
	RecordFile = "machine.json"
	LockFile   = "machine.lock"
	DiskFile   = "disk.raw"
	// ImageUntrustedFile marks a disk.raw that was installed without a
	// checksum (BYO image, no .sha256 sidecar), so an interrupted init
	// that reuses the disk still records image_trusted=false (ADR 0003).
	ImageUntrustedFile = "disk.raw.untrusted"
	SeedFile           = "seed.iso"
	EFIVarsFile        = "efivars.fd"
	SSHDir             = "ssh"
	SSHKeyFile         = "ssh/id_ed25519"
	SSHPubFile         = "ssh/id_ed25519.pub"
	ConsoleFile        = "console.log"
	MachinesDir        = "machines"
	DefaultName        = "jailmachine"
	DefaultImage       = "prebaked"
)

// Guest-side fixed paths (ADR 0003).
const (
	GuestProvisionMarker = "/var/db/jm-provisioned"
	// GuestProvisionFailed is written by provision.sh when it aborts, so
	// that "jm start" can fail fast instead of waiting for the marker.
	GuestProvisionFailed = "/var/db/jm-provision-failed"
	GuestProvisionLog    = "/var/log/jm-provision.log"
	GuestPodmanSocket    = "/var/run/podman/podman.sock"
)

// Machine is the backend-neutral description of a VM. Backend-specific
// tunables live in BackendOpts, namespaced as "backend.<name>.<key>".
//
// Dir is runtime-only: the Store fills it in on Load/Save so that backends
// know where the machine's files live (ADR 0005) without any hypervisor-
// specific plumbing. It is never serialised.
type Machine struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Backend   string `json:"backend"`
	Image     string `json:"image"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memory_mib"`
	DiskGiB   int    `json:"disk_gib"`
	MAC       string `json:"mac"`
	SSHPort   int    `json:"ssh_port"`
	SSHUser   string `json:"ssh_user"`
	// Network is the network provider that created the machine's
	// attachment (ADR 0004). Records written before providers existed have
	// it empty, which means the slirp "user" provider.
	Network string `json:"network,omitempty"`
	// GuestIP is the stable guest address the provider hands out, recorded
	// so that tools can reconnect without the provider being up. It is
	// configuration derived from the provider, not runtime state.
	GuestIP string `json:"guest_ip,omitempty"`
	// PublishAddr is the host address published container ports bind to
	// (ADR 0004). Empty means the default, which is every interface, as
	// "docker run -p" does on Linux. It lives on the record rather than in
	// the environment of whichever shell started the machine, so that the
	// binding a running forwarder uses is the one "jm inspect" shows.
	PublishAddr string `json:"publish_addr,omitempty"`
	// MTU is the link size the provider gave this machine when it was last
	// started. It lives on the record for the same reason PublishAddr
	// does: $JM_MTU is read once, at start, so "jm doctor" and "jm
	// inspect" must report what the running machine actually uses rather
	// than what the current shell would ask for.
	MTU         int       `json:"mtu,omitempty"`
	Created     time.Time `json:"created"`
	Provisioned bool      `json:"provisioned"`
	// ImageTrusted records whether the disk image was verified against a
	// published checksum when fetched (ADR 0003: trust is a property of
	// the source, surfaced uniformly). Records written before the field
	// existed came from the checksummed official source and load as true.
	ImageTrusted bool `json:"image_trusted"`
	// Shares are the host directories offered to the guest at their own
	// absolute path (ADR 0007). The set is reconciled at every start, not
	// frozen at init; an empty set means no host filesystem sharing.
	Shares      []Share           `json:"shares,omitempty"`
	BackendOpts map[string]string `json:"backend_opts,omitempty"`
	Dir         string            `json:"-"`
}

// Defaults returns a Machine populated with the PoC defaults. The caller
// sets Name, Created and Backend (the backend package picks the default per
// host OS; this package stays hypervisor-neutral, ADR 0002).
func Defaults() Machine {
	return Machine{
		Version:      Version,
		Name:         DefaultName,
		Image:        DefaultImage,
		CPUs:         4,
		MemoryMiB:    4096,
		DiskGiB:      64,
		MAC:          "5a:94:ef:e4:0c:ee",
		SSHPort:      2222,
		SSHUser:      "root",
		ImageTrusted: true,
		BackendOpts:  map[string]string{},
	}
}
