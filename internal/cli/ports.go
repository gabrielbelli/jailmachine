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
			"it never blocks on the machine.\n\n" +
			"The host side is bound exactly as docker binds it. \"-p 8080:80\" publishes on\n" +
			"every host interface: 127.0.0.1, ::1, \"localhost\" and the host's LAN address\n" +
			"all reach the container — including from other machines on your network.\n" +
			"'jm set --publish-addr 127.0.0.1' (or $" + forwarder.PublishAddrEnv + " at start time) changes that\n" +
			"default. A publish that names a host address of its own\n" +
			"(\"-p 127.0.0.1:8080:80\", \"-p [::1]:8080:80\") binds that address and only that\n" +
			"one, whatever the default is.\n\n" +
			"The first \"#\" line is the default the running forwarder was started with; a\n" +
			"second one appears when the record has been changed since and is waiting for\n" +
			"a restart.",
		Example: `  jm ports
  jm ports --json dev`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			st := forwardState(m)
			entries := st.Owned
			if JSON() {
				if entries == nil {
					entries = []forwarder.Entry{}
				}
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			_, running := forwarderProcess(m).Alive()
			if !running {
				fmt.Fprintf(stdout, "# port forwarder for %s is not running (jm start%s)\n", m.Name, nameHint(m.Name))
			}
			inForce, pending := publishAddrs(m, running, st)
			fmt.Fprintf(stdout, "# publishing on %s unless -p names a host address\n", inForce)
			if pending != "" {
				fmt.Fprintf(stdout, "# the record says %s; this forwarder keeps %s until jm stop%s && jm start%s\n",
					pending, inForce, nameHint(m.Name), nameHint(m.Name))
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

// remoteOrDash prints "-" for an entry that is never forwarded. Nothing
// podman accepts produces one today; the column keeps its shape for a
// publish shape a future engine might invent.
func remoteOrDash(remote string) string {
	if remote == "" {
		return "-"
	}
	return remote
}
