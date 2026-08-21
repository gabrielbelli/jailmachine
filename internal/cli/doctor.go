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
	if st == backend.Broken {
		res.Status = doctor.Warn
		res.Fix = fmt.Sprintf("stale hypervisor or network state; 'jm stop %s' repairs it (%s)", name, consoleHint(m, b))
		return res
	}
	res.Status = doctor.OK
	return res
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
