package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// info is the inspect/list view: the record plus computed runtime facts.
type info struct {
	*machine.Machine
	State        backend.State `json:"state"`
	BackendState backend.State `json:"backend_state"`
	NetworkState backend.State `json:"network_state"`
	SSH          string        `json:"ssh"`
	SSHKey       string        `json:"ssh_key"`
	Console      string        `json:"console,omitempty"`
	Dir          string        `json:"dir"`
	Podman       string        `json:"podman_uri"`
	PodmanSock   string        `json:"podman_sock_uri,omitempty"`
	APISocket    string        `json:"api_socket,omitempty"`
	DNS          string        `json:"dns,omitempty"`
	NetworkLogs  []string      `json:"network_logs,omitempty"`
	// Ports is the forwarder's owned mapping table with per-mapping errors
	// (ADR 0004); Forwarder is whether the loop is running.
	Ports        []forwarder.Entry `json:"ports"`
	Forwarder    backend.State     `json:"forwarder_state"`
	ForwarderLog string            `json:"forwarder_log,omitempty"`
	// PublishAddr is the host address published ports bind to, resolved
	// (the record's value or the default); the record's own publish_addr
	// is omitted when unset.
	PublishAddr string `json:"publish_addr_effective"`
	// FileSharing is whether the backend can export the machine's Shares
	// (ADR 0007); false with shares configured means they are ignored.
	FileSharing bool `json:"file_sharing"`
	// Resolver is the host-side name-resolution helper of ADR 0008 and
	// ResolverAddr the host address it answers the guest's queries on.
	Resolver     backend.State `json:"resolver_state"`
	ResolverAddr string        `json:"resolver_addr,omitempty"`
	ResolverLog  string        `json:"resolver_log,omitempty"`
	// Autostart is whether "jpodman"/"jdocker" would boot this machine on
	// demand; DockerHost is what they point the docker CLI at.
	Autostart     bool   `json:"autostart"`
	DockerHost    string `json:"docker_host,omitempty"`
	networkString string
}

// describe computes the runtime view of m; read-only, never blocks.
func describe(m *machine.Machine) info {
	i := info{
		Machine: m, State: backend.Broken, BackendState: backend.Broken, NetworkState: backend.Broken,
		SSH: m.SSHEndpoint(), SSHKey: sshKey(m), Dir: store().Dir(m.Name), Podman: m.PodmanURI(),
		networkString: m.NetworkName(),
		Ports:         []forwarder.Entry{},
		Forwarder:     backend.Stopped,
		Resolver:      backend.Stopped,
	}
	if fw := forwards(m); fw != nil {
		i.Ports = fw
	}
	i.PublishAddr = forwarder.HostIP(m.PublishAddr)
	i.Autostart = autostartEnabled()
	if m.Dir != "" {
		pr := forwarderProcess(m)
		i.ForwarderLog = pr.LogPath()
		if _, ok := pr.Alive(); ok {
			i.Forwarder = backend.Running
		}
		rp := resolverProcess(m)
		i.ResolverLog, i.ResolverAddr, i.Resolver = rp.LogPath(), rp.Addr(), resolverState(m)
	}
	b, p, err := components(m)
	if err != nil {
		return i
	}
	i.FileSharing = b.Capabilities().FileSharing
	if st, err := b.State(m); err == nil {
		i.BackendState = st
	}
	if st, err := p.State(m); err == nil {
		i.NetworkState = st
	}
	i.State = combineState(i.BackendState, i.NetworkState, p.Capabilities().Supervised)
	i.Console = b.ConsolePath(m)
	i.NetworkLogs = p.Logs(m)
	if ep, err := p.Endpoint(m); err == nil {
		i.SSH = fmt.Sprintf("%s:%d", ep.SSHHost, ep.SSHPort)
		i.APISocket = ep.APISocket
		i.PodmanSock = machine.SocketURI(ep.APISocket)
		i.DockerHost = i.PodmanSock
		i.DNS = ep.DNS
	}
	return i
}

// inspectLong documents the --json keys; keep it in step with info and
// machine.Machine.
const inspectLong = `Show a machine's record plus its computed runtime state. Nothing is cached:
state is read from the hypervisor and the network provider on every call.

--json prints one object with snake_case keys:

  name, state (running|stopped|broken), backend_state, network_state,
  backend, network, image, cpus, memory_mib, disk_gib, mac, ssh_port,
  ssh_user, guest_ip, ssh (host:port), ssh_key, podman_uri,
  podman_sock_uri, api_socket, dns, console, network_logs, dir,
  provisioned, image_trusted (false for a BYO image without a .sha256 sidecar),
  created, version, backend_opts, file_sharing, autostart, docker_host,
  shares (array of {host_path, guest_path, read_only, tag}),
  ports (array of {proto, local, remote, since, error}), forwarder_state,
  forwarder_log, publish_addr (only when set) and publish_addr_effective,
  resolver_state, resolver_addr, resolver_log.

Keys whose value is empty are omitted.`

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [name]",
		Short: "Show a machine's configuration and state",
		Long:  inspectLong,
		Example: `  jm inspect
  jm inspect --json dev | jq -r .ssh
  jm inspect --json | jq -r .podman_sock_uri`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			i := describe(m)
			if JSON() {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(i)
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			row := func(k string, v any) { fmt.Fprintf(tw, "%s:\t%v\n", k, v) }
			row("Name", i.Name)
			row("State", i.State)
			if i.State == backend.Broken {
				row("  Hypervisor", i.BackendState)
				row("  Network", i.NetworkState)
			}
			row("Backend", i.Backend)
			row("Network", i.networkString)
			row("Image", i.Image)
			row("Image trusted", i.ImageTrusted)
			row("CPUs", i.CPUs)
			row("Memory", fmt.Sprintf("%d MiB", i.MemoryMiB))
			row("Disk", fmt.Sprintf("%d GiB", i.DiskGiB))
			row("MAC", i.MAC)
			if i.GuestIP != "" {
				row("Guest IP", i.GuestIP)
			}
			if i.DNS != "" {
				row("DNS", i.DNS)
			}
			row("SSH", fmt.Sprintf("%s@%s", i.SSHUser, i.SSH))
			row("SSH key", i.SSHKey)
			row("Podman", fmt.Sprintf("%s (%s)", i.Podman, i.Name))
			if i.PodmanSock != "" {
				row("Podman socket", fmt.Sprintf("%s (%s)", i.PodmanSock, i.SocketConnectionName()))
			}
			if i.DockerHost != "" {
				row("Docker host", fmt.Sprintf("%s (%s)", i.DockerHost, DockerWrapperName))
			}
			row("Autostart", autostartWord(i.Autostart))
			if i.Console != "" {
				row("Console", i.Console)
			}
			for _, l := range i.NetworkLogs {
				row("Network log", l)
			}
			row("Resolver", i.Resolver)
			if i.ResolverAddr != "" {
				row("Resolver address", i.ResolverAddr)
			}
			if i.ResolverLog != "" {
				row("Resolver log", i.ResolverLog)
			}
			row("Publish address", i.PublishAddr)
			row("Forwarder", i.Forwarder)
			if i.ForwarderLog != "" {
				row("Forwarder log", i.ForwarderLog)
			}
			for _, e := range i.Ports {
				row("Port", fmt.Sprintf("%s -> %s %s (%s)", e.Local, remoteOrDash(e.Remote), e.Proto, e.Status()))
			}
			for _, sh := range i.Shares {
				v := sh.String()
				if !i.FileSharing {
					v += fmt.Sprintf(" — ignored: backend %q cannot share host directories", i.Backend)
				} else if _, err := os.Stat(sh.HostPath); err != nil {
					v += " — missing on the host, not shared"
				}
				row("Share", v)
			}
			row("Dir", i.Dir)
			row("Provisioned", i.Provisioned)
			row("Created", i.Created.Format("2006-01-02T15:04:05Z07:00"))
			return tw.Flush()
		},
	}
}
