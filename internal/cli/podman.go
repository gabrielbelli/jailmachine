package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
)

// WrapperName is the alternative binary name (symlink to jm) that behaves
// like podman pointed at a jailmachine: "jpodman run ..." == "jm podman run ...".
const WrapperName = "jpodman"

// newPodmanCmd runs the host podman client against a machine without
// changing the user's default podman connection (ADR 0001: jm only hands
// clients an endpoint). Machine selection: $JM_MACHINE, else the default
// resolution (the "jailmachine" machine, or the only one that exists).
func newPodmanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "podman [podman args...]",
		Short: "Run podman against a machine (also available as the jpodman binary)",
		Long: "Execs the host podman with --connection <machine> prepended, leaving the\n" +
			"user's default podman connection untouched. Select the machine with $JM_MACHINE.",
		Example: `  jm podman run --rm --os=linux docker.io/alpine echo hi
  jpodman build -t myapp .
  JM_MACHINE=dev jpodman ps`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return execPodman(args)
		},
	}
	// Stop parsing at the first positional ("run", "build", ...) so podman's
	// own flags reach podman, while jm's global flags before the subcommand
	// (jm --state-root DIR podman ps) are still honoured.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func execPodman(args []string) error {
	var nameArgs []string
	if n := os.Getenv("JM_MACHINE"); n != "" {
		nameArgs = []string{n}
	}
	name, err := resolveName(nameArgs)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("podman")
	if err != nil {
		return errors.New("podman not found on PATH (brew install podman)")
	}
	argv := append([]string{bin, "--connection", name}, args...)
	return syscall.Exec(bin, argv, os.Environ())
}

// wrapperArgs rewrites os.Args when jm is invoked through the jpodman
// symlink so that "jpodman X" becomes "jm podman X".
func wrapperArgs(args []string) []string {
	if len(args) > 0 && filepath.Base(args[0]) == WrapperName {
		return append([]string{args[0], "podman"}, args[1:]...)
	}
	return args
}
