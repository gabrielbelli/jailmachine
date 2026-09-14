package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/doctor"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/version"
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the host for everything jm needs",
		Long:  "doctor checks the host tools (qemu, gvproxy, podman, ssh), the state root and every\nmachine record, printing a fix hint for each problem. Exit status is 1 when any check fails.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rep := doctor.Run(cmd.Context(), doctor.Options{
				StateRoot: StateRoot(),
				Machines:  machineChecks,
			})
			rep.Results = append(rep.Results, runExtraChecks(cmd.Context())...)
			rep.Version = version.Version
			var err error
			if JSON() {
				err = doctor.WriteJSON(stdout, rep)
			} else {
				// The version comes first so a pasted report says which
				// jm produced it.
				fmt.Fprintln(stdout, version.Full())
				fmt.Fprintln(stdout)
				err = doctor.WriteTable(stdout, rep)
			}
			if err != nil {
				return err
			}
			if _, _, fail := rep.Counts(); fail > 0 {
				return fmt.Errorf("%d check(s) failed", fail)
			}
			return nil
		},
	}
}

// machineChecks produces one result per machine directory under the state
// root: the record must load, its backend and provider must be known, and
// the combined state must not be broken (ADR 0005). Directories that
// Store.List would skip (unreadable records) are reported, not hidden.
func machineChecks(ctx context.Context) []doctor.Result {
	entries, err := os.ReadDir(filepath.Join(StateRoot(), machine.MachinesDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []doctor.Result{{Name: "machines", Status: doctor.Fail, Detail: err.Error(), Fix: "fix permissions on the state root"}}
	}
	var out []doctor.Result
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, checkMachine(e.Name()))
		m, err := store().Load(e.Name())
		// Parity — of name resolution (ADR 0008) and of the shared
		// directories (ADR 0007) — is a property of a running machine,
		// so it is only asserted for one. Both check the thing the ADR
		// promises rather than that a component is alive: a resolver
		// that answers with the wrong address, or a share the backend
		// attaches and the guest never mounts, is invisible otherwise.
		if err == nil {
			if res, ok := resolverParityCheck(ctx, m); ok {
				out = append(out, res)
			}
			if res, ok := guestResolverParityCheck(ctx, m); ok {
				out = append(out, res)
			}
		}
		out = append(out, checkMachineShares(e.Name())...)
		if err == nil {
			if res, ok := sharesParityCheck(ctx, m); ok {
				out = append(out, res)
			}
			if res, ok := datagramLimitCheck(m); ok {
				out = append(out, res)
			}
			if res, ok := suspendReadinessCheck(m); ok {
				out = append(out, res)
			}
			if res, ok := sleeperCheck(m); ok {
				out = append(out, res)
			}
		}
	}
	return out
}

// checkMachineShares reports on host filesystem sharing (ADR 0007): every
// shared host path must still be a directory on this host, and the
// machine's backend must be able to export it at all. Both are warnings:
// an unplugged disk is dropped at start, it does not stop the machine. It
// lists the roots, so a path that is not covered by any of them — /tmp/...
// and anything outside the shared trees — can be seen at a glance.
// Whether the guest actually mounts them is sharesParityCheck's job.
func checkMachineShares(name string) []doctor.Result {
	m, err := store().Load(name)
	if err != nil || len(m.Shares) == 0 {
		return nil
	}
	res := doctor.Result{Name: "shares " + name, Status: doctor.OK}
	if b, err := backendFor(m); err == nil && !b.Capabilities().FileSharing {
		res.Status = doctor.Warn
		res.Detail = fmt.Sprintf("%d share(s) configured, backend %q cannot export them", len(m.Shares), b.Name())
		res.Fix = "remove them with 'jm set --unmount <path>" + nameHint(name) + "' or use a backend that shares host directories"
		return []doctor.Result{res}
	}
	ok, skipped := machine.UsableShares(m.Shares)
	var paths []string
	for _, s := range ok {
		paths = append(paths, s.HostPath+" ("+s.Mode()+")")
	}
	res.Detail = fmt.Sprintf("%d share(s) at their host path: %s", len(ok), strings.Join(paths, ", "))
	if len(skipped) > 0 {
		var missing []string
		for _, s := range skipped {
			missing = append(missing, s.Share.HostPath+" ("+s.Reason+")")
		}
		res.Status = doctor.Warn
		res.Detail = fmt.Sprintf("%d of %d share(s) unavailable: %s", len(skipped), len(m.Shares), strings.Join(missing, ", "))
		res.Fix = "plug the volume back in, or 'jm set --unmount <path>" + nameHint(name) + "'"
	}
	return []doctor.Result{res}
}

func checkMachine(name string) doctor.Result {
	res := doctor.Result{Name: "machine " + name}
	m, err := store().Load(name)
	if err != nil {
		res.Status, res.Detail = doctor.Fail, err.Error()
		res.Fix = fmt.Sprintf("remove the half-initialised directory with 'jm rm %s'", name)
		return res
	}
	b, p, err := components(m)
	if err != nil {
		res.Status, res.Detail, res.Fix = doctor.Fail, err.Error(), "this jm build does not know the machine's backend or network; upgrade jm or 'jm rm "+name+"'"
		return res
	}
	st, err := stateOf(m, b, p)
	if err != nil {
		res.Status, res.Detail, res.Fix = doctor.Fail, err.Error(), "jm stop "+name
		return res
	}
	res.Detail = fmt.Sprintf("%s (%s, %s)", st, b.Name(), p.Name())
	switch {
	case st == backend.Suspended:
		// Reported from the journal alone: doctor never wakes a machine.
		res.Status = doctor.OK
		res.Detail = "suspended"
		if s, ok := b.(backend.Suspender); ok {
			if ss, err := s.SuspendStatus(m); err == nil {
				if !ss.SavedAt.IsZero() {
					res.Detail += " since " + ss.SavedAt.Local().Format("2006-01-02 15:04")
				}
				if ss.AllocatedBytes > 0 {
					res.Detail += ", image " + humanBytes(ss.AllocatedBytes)
				}
			}
		}
		res.Detail += fmt.Sprintf(" (wakes on first use; %s, %s)", b.Name(), p.Name())
		return res
	case st == backend.Running && suspendInProgress(m):
		res.Status = doctor.Warn
		res.Detail = fmt.Sprintf("a suspend or wake is in progress or was interrupted (%s, %s)", b.Name(), p.Name())
		res.Fix = "if no jm command is running, 'jm start " + name + "' resolves it"
		return res
	}
	if st == backend.Broken {
		res.Status = doctor.Warn
		res.Fix = fmt.Sprintf("stale hypervisor or network state; 'jm stop %s' repairs it (%s)", name, consoleHint(m, b))
		return res
	}
	res.Status = doctor.OK
	return res
}

// suspendReadinessCheck warns, for a running machine with idle suspend on,
// about what would keep it from being suspended: a hypervisor started by an
// older jm, which cannot be saved without crashing it, or a volume without
// room for the saved state (ADR 0009). It reads files and the process table
// only.
func suspendReadinessCheck(m *machine.Machine) (doctor.Result, bool) {
	res := doctor.Result{Name: "suspend " + m.Name}
	if m.IdleSuspendMin == 0 {
		return res, false
	}
	b, p, err := components(m)
	if err != nil {
		return res, false
	}
	if st, err := stateOf(m, b, p); err != nil || !ready(m, st) {
		return res, false
	}
	if _, ok := b.(backend.Suspender); !ok || !b.Capabilities().Suspend {
		return res, false
	}
	// A provider that cannot be parked makes suspend unsupported, not
	// something to fix: no row, as for a backend without the capability.
	if _, ok := p.(netprov.APIForwarder); !ok || !p.Capabilities().Supervised {
		return res, false
	}
	if err := suspendPreflight(m, b, p); err != nil {
		res.Status, res.Detail = doctor.Warn, err.Error()
		switch {
		case strings.Contains(err.Error(), "free"):
			res.Fix = "free space on the volume holding " + m.Dir + ", or lower --memory"
		case strings.Contains(err.Error(), "older jm"):
			res.Fix = "jm stop " + m.Name + " && jm start " + m.Name
		}
		return res, true
	}
	res.Status, res.Detail = doctor.OK, "can be suspended ('jm suspend "+m.Name+"')"
	return res, true
}

// sleeperCheck reports on the helper that holds a suspended machine's
// endpoints and runs a suspend (ADR 0009), for a running or suspended machine
// whose components can suspend. It reads the pid file and the process table
// only; it never asks the sleeper and never wakes the machine.
func sleeperCheck(m *machine.Machine) (doctor.Result, bool) {
	res := doctor.Result{Name: "sleeper " + m.Name}
	b, p, err := components(m)
	if err != nil || m.Dir == "" || !sleeperSupported(b, p) {
		return res, false
	}
	st, err := stateOf(m, b, p)
	if err != nil {
		return res, false
	}
	pr := sleeperProcess(m)
	_, alive := pr.Alive()
	switch {
	case st == backend.Suspended && alive:
		res.Status, res.Detail = doctor.OK, "holding the engine socket and the SSH port; a connection there wakes the machine"
	case st == backend.Suspended:
		res.Status = doctor.Warn
		res.Detail = "not running: jpodman, jdocker, 'jm start' and 'jm ssh' still wake the machine, but clients of the engine socket or the SSH port are refused"
		res.Fix = "jm start " + m.Name
	case ready(m, st) && alive:
		res.Status, res.Detail = doctor.OK, "running (log: "+pr.LogPath()+")"
	case ready(m, st):
		res.Status = doctor.Warn
		res.Detail = "not running: 'jm suspend' starts it, and a suspended machine's endpoints would not wake it"
		res.Fix = "jm start " + m.Name
	default:
		return res, false
	}
	return res, true
}

// datagramLimitCheck states the one silent limit of the host<->guest link:
// it does not fragment, so a UDP datagram bigger than the provider's MTU
// less its headers is dropped without an error at either end. TCP never
// meets it — the stack segments to fit — and it is invisible in a packet
// capture on the sending side, so someone whose 4 kB datagrams vanish has
// nothing to go on. It is reported as OK rather than a warning because
// nothing is wrong: it is a number to design against, and a pasted report
// should carry it.
//
// The number comes from the machine's record when it has one, so a machine
// started with a different $JM_MTU is reported as it actually runs; a machine
// that has never started falls back to what this environment would give it.
func datagramLimitCheck(m *machine.Machine) (doctor.Result, bool) {
	res := doctor.Result{Name: "datagram limit " + m.Name}
	p, err := providerFor(m)
	if err != nil {
		return res, false // checkMachine already reports an unknown provider
	}
	caps := p.Capabilities()
	mtu, max := m.MTU, 0
	if mtu > 0 {
		max = netprov.Capabilities{MTU: mtu}.MaxDatagram()
	} else {
		mtu, max = caps.MTU, caps.MaxDatagram()
	}
	if max == 0 {
		return res, false // the provider does not say; do not invent one
	}
	res.Status = doctor.OK
	res.Detail = fmt.Sprintf("published udp carries payloads up to %d bytes (%s MTU %d); larger datagrams are dropped, not fragmented. $JM_MTU changes the link size (576..16384; JM_MTU=1500 matches Docker)",
		max, p.Name(), mtu)
	return res, true
}
