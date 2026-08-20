package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

func newEnvCmd() *cobra.Command {
	var shell string
	cmd := &cobra.Command{
		Use:   "env [name]",
		Short: "Print shell exports pointing podman/docker clients at a machine",
		Long: "Print the environment a podman or docker client needs to talk to the machine's\n" +
			"engine through the network provider's host-side unix socket. Requires a\n" +
			"provider that proxies the API socket (gvproxy); the slirp \"user\" provider\n" +
			"only offers the ssh:// connection.\n\n" +
			"  eval \"$(jm env)\"",
		Example: `  eval "$(jm env)"
  eval (jm env dev --shell fish)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadMachine(args)
			if err != nil {
				return err
			}
			ep, err := endpointOf(m)
			if err != nil {
				return err
			}
			return writeEnv(stdout, m, ep, shell)
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "sh", "output syntax: sh or fish")
	return cmd
}

// writeEnv renders the export lines for the given shell.
func writeEnv(w io.Writer, m *machine.Machine, ep netprov.Endpoint, shell string) error {
	var format string
	switch shell {
	case "sh", "bash", "zsh", "":
		format = "export %s=%q\n"
	case "fish":
		format = "set -gx %s %q;\n"
	default:
		return usagef("unknown --shell %q (use sh or fish)", shell)
	}
	if ep.APISocket == "" {
		return fmt.Errorf("network provider %q exposes no API socket for %s; use 'podman --connection %s' over ssh, or re-create the machine with JM_NETWORK=gvproxy",
			m.NetworkName(), m.Name, m.Name)
	}
	// podman resolves CONTAINER_CONNECTION before CONTAINER_HOST, so it
	// must name the socket connection, not the ssh:// one.
	uri := machine.SocketURI(ep.APISocket)
	vars := [][2]string{
		{"CONTAINER_HOST", uri},
		{"CONTAINER_CONNECTION", m.SocketConnectionName()},
		{"DOCKER_HOST", uri},
	}
	for _, v := range vars {
		fmt.Fprintf(w, format, v[0], v[1])
	}
	if shell == "fish" {
		fmt.Fprintf(w, "# run: eval (jm env%s --shell fish)\n", nameHint(m.Name))
	} else {
		fmt.Fprintf(w, "# run: eval \"$(jm env%s)\"\n", nameHint(m.Name))
	}
	return nil
}
