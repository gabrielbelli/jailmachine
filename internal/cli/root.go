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
		Long:          "jailmachine (jm) provisions and manages a FreeBSD virtual machine for running\njails (bastille) and OCI containers (podman), and connects the host's podman client to it.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Backends and podman receive paths from the state root; make it
		// absolute once so they do not depend on the working directory.
		PersistentPreRunE: func(*cobra.Command, []string) error {
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
	)
	return root
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := NewRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "jm: %v\n", err)
		os.Exit(1)
	}
}
