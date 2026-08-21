package cli

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the jm version, commit and build date",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if JSON() {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(version.Current())
			}
			return version.Write(stdout)
		},
	}
}
