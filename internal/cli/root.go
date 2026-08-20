// Package cli wires the cobra command tree for jm. One file per subcommand.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	// Backends register themselves with internal/backend on import; this is
	// the only place the CLI names one (ADR 0002).
	_ "github.com/gabrielbelli/jailmachine/internal/backend/qemu"

	// Network providers likewise register themselves (ADR 0004).
	_ "github.com/gabrielbelli/jailmachine/internal/netprov/gvproxy"
	_ "github.com/gabrielbelli/jailmachine/internal/netprov/user"
)

// Global flag values, shared by every subcommand.
var (
	stateRoot string
	jsonOut   bool
)

// StateRoot returns the resolved --state-root directory.
func StateRoot() string { return stateRoot }

// JSON reports whether --json was requested.
func JSON() bool { return jsonOut }

func defaultStateRoot() string {
	if v := os.Getenv("JM_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".jailmachine"
	}
	return filepath.Join(home, ".jailmachine")
}

// NewRootCmd builds the root command with all subcommands registered.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "jm",
		Short:         "Manage a FreeBSD VM for jails and OCI containers",
		Long:          rootLong,
		Example:       rootExample,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Unknown subcommands are usage errors (exit 2), not failures: a
		// root without Run would print help and exit 0 instead.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
		// Backends and podman receive paths from the state root; make it
		// absolute once so they do not depend on the working directory.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			activeCommand, activeMachine = cmd.Name(), ""
			abs, err := filepath.Abs(stateRoot)
			if err != nil {
				return fmt.Errorf("resolving --state-root %q: %w", stateRoot, err)
			}
			stateRoot = abs
			return nil
		},
	}
	root.PersistentFlags().StringVar(&stateRoot, "state-root", defaultStateRoot(), "directory holding machine state")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON output")
	root.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "suppress stage lines and progress (errors still go to stderr)")
	// Bad flags are usage errors too.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usage(err) })

	root.AddCommand(
		newInitCmd(),
		newStartCmd(),
		newStopCmd(),
		newSSHCmd(),
		newInspectCmd(),
		newRmCmd(),
		newListCmd(),
		newEnvCmd(),
		newPortsCmd(),
		newForwarderCmd(),
		newDoctorCmd(),
		newSetCmd(),
		newConsoleCmd(),
	)
	markUsageErrors(root)
	return root
}

// markUsageErrors wraps every command's positional-argument validator so
// "accepts at most 1 arg" and "unknown command" exit 2 rather than 1.
func markUsageErrors(cmd *cobra.Command) {
	if v := cmd.Args; v != nil {
		cmd.Args = func(c *cobra.Command, args []string) error { return usage(v(c, args)) }
	}
	for _, sub := range cmd.Commands() {
		markUsageErrors(sub)
	}
}

const rootLong = `jailmachine (jm) provisions and manages a FreeBSD virtual machine for running
jails (bastille) and OCI containers (podman), and connects the host's podman
client to it.

Quickstart (three commands):

  jm init                      # download the FreeBSD image, write the seed
  jm start                     # boot, provision on first boot, connect podman
  podman run --rm --os=linux docker.io/alpine echo hi

Linux images run through the Linuxulator and need --os=linux on the host
podman (or "podman pull --os=linux"); native FreeBSD images need nothing.

Commands that take an optional [name] default to "jailmachine"; when that
does not exist and exactly one machine does, that one is used. Exit codes:
0 ok, 1 failure, 2 usage error. Errors name the log file to read.`

const rootExample = `  jm init && jm start
  jm list
  jm ssh -- uname -a
  jm stop && jm rm`

// Execute runs the root command and exits with ExitOK, ExitFailure or
// ExitUsage. Errors are printed as "jm: <command> <name>: <stage>: <cause>"
// with a hint line naming what to read or do.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := NewRootCmd()
	cmd, err := root.ExecuteContextC(ctx)
	if err != nil {
		command := ""
		if cmd != nil && cmd != root {
			command = cmd.Name()
		}
		fmt.Fprintln(os.Stderr, formatError(command, activeMachine, err))
		os.Exit(exitCode(err))
	}
}
