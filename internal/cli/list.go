package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List machines",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ms, err := store().List()
			if err != nil {
				return err
			}
			infos := make([]info, 0, len(ms))
			for _, m := range ms {
				infos = append(infos, describe(m))
			}
			if JSON() {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(infos)
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tCPUS\tMEMORY\tDISK\tSSH\tPORTS")
			for _, i := range infos {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d MiB\t%d GiB\t%s\t%d\n", i.Name, i.State, i.CPUs, i.MemoryMiB, i.DiskGiB, i.SSH, len(i.Ports))
			}
			return tw.Flush()
		},
	}
}
