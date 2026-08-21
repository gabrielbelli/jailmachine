package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// MachineEnv names the machine the client wrappers talk to, overriding the
// default resolution.
const MachineEnv = "JM_MACHINE"

// WrapperName is the alternative binary name (symlink to jm) that behaves
// like podman pointed at a jailmachine: "jpodman run ..." == "jm podman run ...".
const WrapperName = "jpodman"

// newPodmanCmd runs the host podman client against a machine without
// changing the user's default podman connection (ADR 0001: jm only hands
// clients an endpoint). Machine selection: $JM_MACHINE, else the default
// resolution (the "jailmachine" machine, or the only one that exists).
// A stopped machine is started first; see autostart.go.
func newPodmanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "podman [" + NoAutostartFlag + "] [podman args...]",
		Short: "Run podman against a machine (also available as the jpodman binary)",
		Long: "Execs the host podman with --connection <machine> prepended, leaving the\n" +
			"user's default podman connection untouched. Select the machine with $JM_MACHINE.\n\n" +
			"A machine that is not running is started first, with one line on stderr;\n" +
			"pass " + NoAutostartFlag + " as the first argument, or set " + AutostartEnv + "=0, to fail instead.",
		Example: `  jm podman run --rm --os=linux docker.io/alpine echo hi
  jpodman build -t myapp .
  JM_MACHINE=dev jpodman ps
  jpodman ` + NoAutostartFlag + ` ps`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return execPodman(cmd.Context(), args)
		},
	}
}

func execPodman(ctx context.Context, args []string) error {
	name, rest, err := wrapperTarget(ctx, args)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("podman")
	if err != nil {
		return withHint(errors.New("podman not found on PATH"), "brew install podman")
	}
	env := os.Environ()
	conn := name
	// "podman compose" hands the work to an external provider (Docker
	// Compose or podman-compose) and passes it the connection's URI. The
	// ssh:// URI podman uses would send the provider looking for a docker
	// daemon in the guest, so point it at the socket connection instead,
	// which is a plain unix:// path on this Mac (ADR 0004: the provider
	// proxies the guest API to a host socket).
	if isComposeCall(rest) {
		if m, err := store().Load(name); err == nil {
			if ep, err := endpointOf(m); err == nil && ep.APISocket != "" {
				conn = m.SocketConnectionName()
				uri := machine.SocketURI(ep.APISocket)
				env = append(env, "DOCKER_HOST="+uri, "CONTAINER_HOST="+uri)
			}
		}
	}
	argv := append([]string{bin, "--connection", conn}, rest...)
	return syscall.Exec(bin, argv, env)
}

// wrapperTarget is the shared preamble of the client wrappers: pick the
// machine ($JM_MACHINE, else the default resolution), make sure it is
// running, and hand back the arguments to pass on. Stage lines go to stderr
// here, whatever they normally do: stdout belongs to the client command.
func wrapperTarget(ctx context.Context, args []string) (name string, rest []string, err error) {
	rest, noAutostart := splitAutostartFlag(args)
	defer toStderr()()
	var nameArgs []string
	if n := os.Getenv(MachineEnv); n != "" {
		nameArgs = []string{n}
	}
	name, err = resolveName(nameArgs)
	if err != nil {
		return "", nil, err
	}
	if !clientOnly(rest) {
		if err := ensureRunning(ctx, name, autostartEnabled() && !noAutostart); err != nil {
			return "", nil, err
		}
	}
	return name, rest, nil
}

// toStderr sends the stage lines to stderr until the returned func is
// called.
func toStderr() func() {
	saved := stdout
	stdout = stderr
	return func() { stdout = saved }
}

// wrapperArgs rewrites os.Args when jm is invoked through one of its
// client-wrapper symlinks, so that "jpodman X" becomes "jm podman X" and
// "jdocker X" becomes "jm docker X".
//
// An invocation naming an internal subcommand is left alone: jm launches
// its own detached helpers ("_forwarder", "_resolver") with the executable
// path it was started from, which is the symlink when the user ran a
// wrapper. Without this a helper would come back as "jpodman ... _forwarder
// <name>" and hand its arguments to podman.
func wrapperArgs(root *cobra.Command, args []string) []string {
	if len(args) == 0 || namesInternalCommand(root, args[1:]) {
		return args
	}
	switch filepath.Base(args[0]) {
	case WrapperName:
		return append([]string{args[0], "podman"}, args[1:]...)
	case DockerWrapperName:
		return append([]string{args[0], "docker"}, args[1:]...)
	}
	return args
}

// namesInternalCommand reports whether args mention one of jm's hidden
// subcommands, wherever it sits among the global flags.
func namesInternalCommand(root *cobra.Command, args []string) bool {
	for _, sub := range root.Commands() {
		if !sub.Hidden {
			continue
		}
		for _, a := range args {
			if a == sub.Name() {
				return true
			}
		}
	}
	return false
}

// jmBinary is the path detached helpers are launched from: the running
// executable with any symlink resolved, so a helper started from a
// "jpodman" or "jdocker" invocation still runs as jm.
func jmBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		return real, nil
	}
	return exe, nil
}

// isComposeCall reports whether these wrapper arguments invoke the compose
// shim, i.e. whether the first non-flag argument is "compose".
func isComposeCall(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a == "compose"
	}
	return false
}
