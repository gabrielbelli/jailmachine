// Package doctor runs host health checks for "jm doctor": are the tools
// jm shells out to installed, new enough and able to do what jm asks of
// them, is the state root usable, and is every machine record consistent.
//
// The host checks live here; the per-machine checks are contributed by the
// CLI, which owns the state-combination logic (ADR 0005), through
// Options.Machines. Rendering is separate from checking so both can be
// unit-tested with fake results.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/backend/qemu"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/netprov/gvproxy"
	"github.com/gabrielbelli/jailmachine/internal/resolver"
)

// Status is the outcome of one check.
type Status string

// Check outcomes. Fail makes "jm doctor" exit non-zero; Warn does not.
const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Result is one check's outcome: what was checked, what was found and, when
// it is not OK, a one-line fix hint.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Report is the full set of results plus host facts worth seeing at a glance.
type Report struct {
	// Version is the jm build that produced the report (filled by the CLI).
	Version   string   `json:"version,omitempty"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	StateRoot string   `json:"state_root"`
	Results   []Result `json:"checks"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	_, _, fail := r.Counts()
	return fail > 0
}

// Counts returns how many checks ended in each status.
func (r Report) Counts() (ok, warn, fail int) {
	for _, c := range r.Results {
		switch c.Status {
		case OK:
			ok++
		case Warn:
			warn++
		case Fail:
			fail++
		}
	}
	return ok, warn, fail
}

// Minimum versions jm is tested against.
const (
	MinQEMUMajor   = 8
	MinPodmanMajor = 5
)

// Runner executes a binary and returns its trimmed combined output.
type Runner func(ctx context.Context, bin string, args ...string) (string, error)

// Options parametrise Run.
type Options struct {
	// StateRoot is the resolved --state-root.
	StateRoot string
	// Machines, when set, contributes per-machine results; the CLI owns
	// the machine store and the state-combination logic.
	Machines func(ctx context.Context) []Result
	// Run executes binaries; nil means os/exec. Tests substitute a fake.
	Run Runner
}

// Run executes every check and returns the report. It never returns an
// error: problems are results.
func Run(ctx context.Context, o Options) Report {
	if o.Run == nil {
		o.Run = execRun
	}
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH, StateRoot: o.StateRoot}
	add := func(res ...Result) { r.Results = append(r.Results, res...) }

	add(checkHost())
	add(checkHostResolver())
	add(checkQEMU(ctx, o.Run)...)
	add(checkGvproxy(ctx, o.Run))
	add(checkPodman(ctx, o.Run))
	add(checkBinary("ssh", "ssh", "install OpenSSH (xcode-select --install)", Fail))
	add(checkBinary("ssh-keygen", "ssh-keygen", "install OpenSSH (xcode-select --install)", Fail))
	add(checkBinary("xz", "xz", "brew install xz (optional: image decompression falls back to the slower in-process decoder)", Warn))
	add(checkStateRoot(o.StateRoot))
	add(checkSocketPaths(o.StateRoot))
	if o.Machines != nil {
		add(o.Machines(ctx)...)
	}
	return r
}

// checkHostResolver reports whether this jm can reach the host operating
// system's resolver. Without it jm answers guest queries from Go's own DNS
// client, which sees neither scoped (VPN) resolvers nor /etc/hosts nor
// .local: public names keep working and everything private stops, which is
// exactly the invisible regression ADR 0008 refuses to allow.
//
// It asks resolver.SystemMode rather than the build tag alone, because
// GODEBUG=netdns=go gives the path up at run time in a build that has it.
// This is still only *this* process; what the resolver serving a machine
// does is asserted over the wire, per machine, by resolverParityCheck.
func checkHostResolver() Result {
	res := Result{Name: "host resolver"}
	if resolver.SystemMode() == resolver.ModeHost {
		res.Status, res.Detail = OK, "queries go through the host resolver (getaddrinfo)"
		return res
	}
	res.Status = Fail
	if resolver.HostResolver {
		res.Detail = "GODEBUG=netdns=go is set: name resolution in the guest would miss VPN, /etc/hosts and .local names"
	} else {
		res.Detail = "built with the netgo build tag: name resolution in the guest would miss VPN, /etc/hosts and .local names"
	}
	res.Fix = "rebuild jm without '-tags netgo' (CGO_ENABLED does not matter on darwin), and do not set GODEBUG=netdns=go"
	return res
}

func execRun(ctx context.Context, bin string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func checkHost() Result {
	res := Result{Name: "host", Detail: runtime.GOOS + "/" + runtime.GOARCH}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		res.Status = OK
		return res
	}
	res.Status = Fail
	res.Fix = "jailmachine currently supports macOS on Apple Silicon only (Linux/KVM is planned)"
	return res
}

// checkQEMU reports on the backend preflight, the emulator version, its
// HVF accelerator and the EDK2 firmware files. The binary and firmware
// lookups are the backend's own (qemu.LookupBinary, qemu.FirmwareDir), not
// a copy of them.
func checkQEMU(ctx context.Context, run Runner) []Result {
	pre := Result{Name: "backend " + qemu.Name, Status: OK, Detail: "preflight passed"}
	if b, err := backend.Get(qemu.Name); err != nil {
		pre.Status, pre.Detail = Fail, err.Error()
	} else if err := b.Preflight(); err != nil {
		pre.Status, pre.Detail, pre.Fix = Fail, err.Error(), "brew install qemu"
	}

	bin, err := qemu.LookupBinary()
	if err != nil {
		// Preflight already reported the missing binary; the dependent
		// checks cannot run without it.
		return []Result{pre}
	}
	ver := checkVersion(ctx, run, qemu.Binary+" version", bin, []string{"--version"}, MinQEMUMajor, "brew upgrade qemu")

	accel := Result{Name: qemu.Binary + " hvf"}
	out, err := run(ctx, bin, "-accel", "help")
	switch {
	case err != nil:
		accel.Status, accel.Detail, accel.Fix = Fail, firstLine(out, err), "reinstall QEMU (brew reinstall qemu)"
	case hasAccel(out, "hvf"):
		accel.Status, accel.Detail = OK, "Hypervisor.framework accelerator available"
	default:
		accel.Status, accel.Detail, accel.Fix = Fail, "hvf not in '-accel help' output", "install a QEMU built with HVF support (brew install qemu)"
	}

	fw := Result{Name: "edk2 firmware"}
	dir, err := qemu.FirmwareDir(bin)
	if err != nil {
		fw.Status, fw.Detail, fw.Fix = Fail, err.Error(), "reinstall QEMU so share/qemu holds the EDK2 images (brew reinstall qemu)"
	} else if missing := missingFiles(dir, qemu.FirmwareCode, qemu.FirmwareVars); len(missing) > 0 {
		fw.Status, fw.Detail, fw.Fix = Fail, "missing "+strings.Join(missing, ", ")+" in "+dir, "reinstall QEMU (brew reinstall qemu)"
	} else {
		fw.Status, fw.Detail = OK, dir
	}
	return []Result{pre, ver, accel, fw}
}

func checkGvproxy(ctx context.Context, run Runner) Result {
	res := Result{Name: "gvproxy"}
	p, err := netprov.Get(gvproxy.Name)
	if err != nil {
		res.Status, res.Detail = Fail, err.Error()
		return res
	}
	if err := p.Preflight(); err != nil {
		res.Status, res.Detail, res.Fix = Fail, err.Error(), "brew install podman (ships gvproxy) or set $"+gvproxy.BinaryEnv
		return res
	}
	bin, err := gvproxy.LookupBinary()
	if err != nil {
		res.Status, res.Detail = Fail, err.Error()
		return res
	}
	out, err := run(ctx, bin, "-version")
	if err != nil {
		res.Status, res.Detail, res.Fix = Warn, bin+": "+firstLine(out, err), "gvproxy did not report a version; reinstall podman if start fails"
		return res
	}
	res.Status = OK
	if v, ok := ParseVersion(out); ok {
		res.Detail = v.String() + " at " + bin
	} else {
		res.Detail = bin
	}
	return res
}

func checkPodman(ctx context.Context, run Runner) Result {
	bin, err := exec.LookPath("podman")
	if err != nil {
		return Result{Name: "podman", Status: Fail, Detail: "not found on PATH", Fix: "brew install podman"}
	}
	return checkVersion(ctx, run, "podman version", bin, []string{"--version"}, MinPodmanMajor, "brew upgrade podman")
}

// checkVersion runs bin with args and requires a parseable version whose
// major is at least min.
func checkVersion(ctx context.Context, run Runner, name, bin string, args []string, min int, upgrade string) Result {
	res := Result{Name: name}
	out, err := run(ctx, bin, args...)
	if err != nil {
		res.Status, res.Detail, res.Fix = Fail, firstLine(out, err), "reinstall "+filepath.Base(bin)
		return res
	}
	v, ok := ParseVersion(out)
	if !ok {
		res.Status, res.Detail, res.Fix = Warn, "could not parse version from: "+firstLine(out, nil), "expected a 'name version X.Y.Z' line"
		return res
	}
	res.Detail = v.String() + " at " + bin
	if v.Major < min {
		res.Status, res.Fix = Fail, fmt.Sprintf("%s (need %d.x or newer)", upgrade, min)
		return res
	}
	res.Status = OK
	return res
}

// checkBinary looks a tool up on PATH; missing reports with the given
// severity.
func checkBinary(name, bin, fix string, missing Status) Result {
	p, err := exec.LookPath(bin)
	if err != nil {
		return Result{Name: name, Status: missing, Detail: "not found on PATH", Fix: fix}
	}
	return Result{Name: name, Status: OK, Detail: p}
}

// checkStateRoot verifies the state root can be written to. An existing
// root gets a probe file; a missing one is fine when its nearest existing
// ancestor is writable (init creates it).
func checkStateRoot(root string) Result {
	res := Result{Name: "state root", Detail: root}
	st, err := os.Stat(root)
	switch {
	case err == nil && !st.IsDir():
		res.Status, res.Fix = Fail, "not a directory: move it aside or pass another --state-root"
		return res
	case errors.Is(err, os.ErrNotExist):
		if err := probeDir(nearestExisting(filepath.Dir(root))); err != nil {
			res.Status, res.Detail, res.Fix = Fail, err.Error(), "create it yourself (mkdir -p) or pass a writable --state-root"
			return res
		}
		res.Status, res.Detail = OK, root+" (will be created by jm init)"
		return res
	case err != nil:
		res.Status, res.Detail, res.Fix = Fail, err.Error(), "fix permissions or pass another --state-root"
		return res
	}
	if err := probeDir(root); err != nil {
		res.Status, res.Detail, res.Fix = Fail, err.Error(), "fix permissions (chmod u+rwx) or pass a writable --state-root"
		return res
	}
	res.Status = OK
	return res
}

func nearestExisting(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func probeDir(dir string) error {
	f, err := os.CreateTemp(dir, ".jm-doctor-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// checkSocketPaths warns when the machine directories are so deep that
// unix sockets will not fit in sun_path and fall back to the temp dir
// (backend.SocketPath). It uses the default machine name plus the longest
// socket file name jm creates; a longer machine name only makes it worse.
func checkSocketPaths(root string) Result {
	dir := machine.NewStore(root).Dir(machine.DefaultName)
	longest := ""
	for _, n := range []string{qemu.QMPSockFile, gvproxy.NetSockFile, gvproxy.APISockFile, gvproxy.PodmanSockFile} {
		if len(n) > len(longest) {
			longest = n
		}
	}
	sock := backend.SocketPath(dir, longest)
	res := Result{Name: "socket paths"}
	if backend.InTree(dir, sock) {
		res.Status, res.Detail = OK, fmt.Sprintf("%d of %d bytes used", len(sock), backend.MaxSocketPath)
		return res
	}
	res.Status = Warn
	res.Detail = fmt.Sprintf("%s exceeds sun_path (%d bytes); sockets go to %s", filepath.Join(dir, longest), backend.MaxSocketPath, filepath.Dir(sock))
	res.Fix = "use a shorter --state-root (or $JM_HOME) and short machine names"
	return res
}

func hasAccel(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
}

func missingFiles(dir string, names ...string) []string {
	var missing []string
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			missing = append(missing, n)
		}
	}
	return missing
}

// firstLine condenses command output (or the exec error) into one line.
func firstLine(out string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(out), "\n"); line != "" {
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "(no output)"
}
