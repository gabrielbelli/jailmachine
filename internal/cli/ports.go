package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/forwarder"
)

func newPortsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ports [name]",
		Short: "List the container ports published on the host",
		Long: "List the host->guest port mappings the forwarder owns for a machine, with\n" +
			"the outcome of the last attempt (a host port already in use shows as an\n" +
			"error and is retried on the next resync). Reads the machine's forwards.json;\n" +
			"it never blocks on the machine.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			entries := forwards(m)
			if JSON() {
				if entries == nil {
					entries = []forwarder.Entry{}
				}
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			if _, alive := forwarderProcess(m).Alive(); !alive {
				fmt.Fprintf(stdout, "# port forwarder for %s is not running (jm start%s)\n", m.Name, nameHint(m.Name))
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "LOCAL\tREMOTE\tPROTO\tSTATUS")
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Local, remoteOrDash(e.Remote), e.Proto, e.Status())
			}
			return tw.Flush()
		},
	}
}

// remoteOrDash prints "-" for an entry that is never forwarded (a port
// podman bound to the guest's loopback only).
func remoteOrDash(remote string) string {
	if remote == "" {
		return "-"
	}
	return remote
}
