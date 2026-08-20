package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// info is the inspect/list view: the record plus computed runtime facts.
type info struct {
	*machine.Machine
	State   backend.State `json:"state"`
	SSH     string        `json:"ssh"`
	SSHKey  string        `json:"ssh_key"`
	Console string        `json:"console,omitempty"`
	Dir     string        `json:"dir"`
	Podman  string        `json:"podman_uri"`
}

// describe computes the runtime view of m; read-only, never blocks.
func describe(m *machine.Machine) info {
	i := info{Machine: m, SSH: m.SSHEndpoint(), SSHKey: sshKey(m), Dir: store().Dir(m.Name), Podman: m.PodmanURI()}
	b, err := backendFor(m)
	if err != nil {
		i.State = backend.Broken
		return i
	}
	if st, err := b.State(m); err == nil {
		i.State = st
	} else {
		i.State = backend.Broken
	}
	i.Console = b.ConsolePath(m)
	return i
}

func newInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [name]",
		Short: "Show a machine's configuration and state",
		Args:  cobra.MaximumNArgs(1),
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
			row("Backend", i.Backend)
			row("Image", i.Image)
			row("CPUs", i.CPUs)
			row("Memory", fmt.Sprintf("%d MiB", i.MemoryMiB))
			row("Disk", fmt.Sprintf("%d GiB", i.DiskGiB))
			row("MAC", i.MAC)
			row("SSH", fmt.Sprintf("%s@%s", i.SSHUser, i.SSH))
			row("SSH key", i.SSHKey)
			row("Podman", i.Podman)
			if i.Console != "" {
				row("Console", i.Console)
			}
			row("Dir", i.Dir)
			row("Provisioned", i.Provisioned)
			row("Created", i.Created.Format("2006-01-02T15:04:05Z07:00"))
			return tw.Flush()
		},
	}
}
