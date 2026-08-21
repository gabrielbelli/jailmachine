package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// DockerWrapperName is the alternative binary name (symlink to jm) that
// behaves like docker pointed at a jailmachine: "jdocker ps" == "jm docker ps".
const DockerWrapperName = "jdocker"

// DockerHostEnv and DockerContextEnv are the docker client's endpoint
// settings. DOCKER_HOST wins over the current context, but a stale
// DOCKER_CONTEXT in the environment is confusing, so the wrapper drops it.
const (
	DockerHostEnv    = "DOCKER_HOST"
	DockerContextEnv = "DOCKER_CONTEXT"
	// DockerPlatformEnv is the docker client's default --platform. The
	// guest engine is FreeBSD, and its Docker-compat API defaults a pull to
	// the server's own OS, so a fresh "jdocker run alpine" asks the registry
	// for OS "freebsd" and fails. podman users answer that with --os=linux;
	// the docker CLI has no such flag (it rejects it outright), so the
	// wrapper answers it here instead. A value the user already set wins,
	// as does an explicit --platform on the command line.
	DockerPlatformEnv = "DOCKER_DEFAULT_PLATFORM"
)

// newDockerCmd runs the real docker client against a machine's engine.
// podman's API socket serves the Docker API too, so the docker CLI, docker
// compose and anything else speaking DOCKER_HOST work unchanged (ADR 0001:
// jm hands clients an endpoint, it does not proxy or rewrite their
// arguments).
func newDockerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "docker [" + NoAutostartFlag + "] [docker args...]",
		Short: "Run docker against a machine (also available as the jdocker binary)",
		Long: "Execs the host docker client with " + DockerHostEnv + " pointing at the machine's\n" +
			"engine socket, leaving your docker contexts alone. Select the machine with\n" +
			"$" + MachineEnv + ".\n\n" +
			"A machine that is not running is started first, with one line on stderr;\n" +
			"pass " + NoAutostartFlag + " as the first argument, or set " + AutostartEnv + "=0, to fail instead.\n\n" +
			"Needs the docker CLI on the host (brew install docker) and a network\n" +
			"provider that proxies the engine API to a host socket (gvproxy).",
		Example: `  jm docker run --rm docker.io/alpine echo hi
  jdocker compose up -d
  JM_MACHINE=dev jdocker ps`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return execDocker(cmd.Context(), args)
		},
	}
}

func execDocker(ctx context.Context, args []string) error {
	name, rest, err := wrapperTarget(ctx, args)
	if err != nil {
		return err
	}
	m, err := store().Load(name)
	if err != nil {
		return err
	}
	host, err := dockerHost(m)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("docker")
	if err != nil {
		return withHint(errors.New("the docker CLI is not on PATH"),
			"brew install docker (the client alone; jm provides the engine), or use 'jpodman'")
	}
	argv := append([]string{bin}, rest...)
	return syscall.Exec(bin, argv, dockerEnv(os.Environ(), host))
}

// dockerHost is the DOCKER_HOST value for a machine: the host-side unix
// socket its network provider proxies to the guest engine API. Providers
// without one (slirp) cannot serve the docker CLI at all, since docker
// speaks no ssh:// transport of podman's kind.
func dockerHost(m *machine.Machine) (string, error) {
	ep, err := endpointOf(m)
	if err != nil {
		return "", err
	}
	if ep.APISocket == "" {
		return "", withHint(fmt.Errorf("network provider %q exposes no API socket for %s", m.NetworkName(), m.Name),
			"re-create the machine with JM_NETWORK=gvproxy, or use 'jpodman' over ssh")
	}
	return machine.SocketURI(ep.APISocket), nil
}

// dockerEnv returns env with DOCKER_HOST set to host and DOCKER_CONTEXT
// removed, so the wrapper is unaffected by whatever context the user's
// docker CLI is pointing at, plus a default platform of linux/<arch> unless
// the caller chose one (see DockerPlatformEnv). Someone building native
// FreeBSD images opts out by exporting DOCKER_DEFAULT_PLATFORM themselves.
func dockerEnv(env []string, host string) []string {
	out := make([]string, 0, len(env)+2)
	platform := false
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, DockerHostEnv+"="), strings.HasPrefix(kv, DockerContextEnv+"="):
			continue
		case strings.HasPrefix(kv, DockerPlatformEnv+"="):
			platform = true
		}
		out = append(out, kv)
	}
	out = append(out, DockerHostEnv+"="+host)
	if !platform {
		out = append(out, DockerPlatformEnv+"=linux/"+runtime.GOARCH)
	}
	return out
}
