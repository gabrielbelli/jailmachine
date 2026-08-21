package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/doctor"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// extraChecks are contributed by feature files through registerCheck and run
// after the host checks in "jm doctor". Registering keeps a feature's checks
// next to the feature instead of in one growing list.
var extraChecks []func(context.Context) []doctor.Result

func registerCheck(f func(context.Context) []doctor.Result) {
	extraChecks = append(extraChecks, f)
}

func runExtraChecks(ctx context.Context) []doctor.Result {
	var out []doctor.Result
	for _, f := range extraChecks {
		out = append(out, f(ctx)...)
	}
	return out
}

func init() { registerCheck(clientChecks) }

// clientChecks report on the two things that make the machine invisible in
// daily use: the client wrappers being installed, and autostart being on.
// Neither is fatal, and both name the command that fixes them.
func clientChecks(context.Context) []doctor.Result {
	out := []doctor.Result{wrapperCheck(WrapperName, "podman"), wrapperCheck(DockerWrapperName, "docker")}

	auto := doctor.Result{Name: "autostart", Status: doctor.OK,
		Detail: "on: " + WrapperName + "/" + DockerWrapperName + " boot a stopped machine"}
	if !autostartEnabled() {
		auto.Detail = "off in this environment ($" + AutostartEnv + "/$" + NoAutostartEnv + ")"
		auto.Fix = "unset " + AutostartEnv + " and " + NoAutostartEnv + " to have the wrappers start the machine"
	}
	return append(out, auto)
}

// wrapperCheck looks for one client wrapper next to the jm binary and on
// PATH. A missing client (docker itself) is reported by the wrapper it
// belongs to, since jm needs neither to work.
func wrapperCheck(wrapper, client string) doctor.Result {
	res := doctor.Result{Name: wrapper}
	path, err := exec.LookPath(wrapper)
	if err != nil {
		res.Status, res.Detail = doctor.Warn, "not on PATH"
		res.Fix = "ln -sf " + selfPath() + " " + filepath.Join(filepath.Dir(selfPath()), wrapper)
		return res
	}
	res.Status, res.Detail = doctor.OK, path
	if _, err := exec.LookPath(client); err != nil {
		res.Status = doctor.Warn
		res.Detail = path + ", but the " + client + " client is not on PATH"
		res.Fix = "brew install " + client
	}
	return res
}

func selfPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "jm"
}

func init() { registerCheck(clockChecks) }

// clockTimeoutDoctor bounds the ssh round trip the clock check costs.
const clockTimeoutDoctor = 15 * time.Second

// clockChecks compare each running machine's clock with the host's, and
// report whether the guest carries the service that keeps them in step. A
// stopped machine has no clock to check and is skipped silently.
func clockChecks(ctx context.Context) []doctor.Result {
	ms, err := store().List()
	if err != nil {
		return nil
	}
	var out []doctor.Result
	for _, m := range ms {
		if st, err := currentState(m); err != nil || st != backend.Running {
			continue
		}
		out = append(out, clockCheck(ctx, m))
	}
	return out
}

func clockCheck(ctx context.Context, m *machine.Machine) doctor.Result {
	res := doctor.Result{Name: "clock " + m.Name}
	ctx, cancel := context.WithTimeout(ctx, clockTimeoutDoctor)
	defer cancel()
	ep, err := endpointOf(m)
	if err != nil {
		res.Status, res.Detail = doctor.Warn, err.Error()
		return res
	}
	client, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		res.Status, res.Detail, res.Fix = doctor.Warn, "cannot ask the guest: "+err.Error(), "jm start"+nameHint(m.Name)
		return res
	}
	defer client.Close()
	gc, err := readGuestClock(ctx, client)
	if err != nil {
		res.Status, res.Detail = doctor.Warn, err.Error()
		return res
	}
	res.Detail = fmt.Sprintf("%s the host", skewWord(gc.Skew))
	switch {
	case abs(gc.Skew) >= ClockSkewThreshold:
		res.Status = doctor.Warn
		res.Fix = "jm start" + nameHint(m.Name) + " steps it; a guest with the jm_rtcsync service does it by itself"
	case !gc.Service:
		res.Status = doctor.Warn
		res.Detail += ", but the guest has no jm_rtcsync service"
		res.Fix = "re-create the machine so its clock follows the host across sleep"
	default:
		res.Status = doctor.OK
		res.Detail += " (jm_rtcsync running)"
	}
	return res
}
