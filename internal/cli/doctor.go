package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/doctor"
	"github.com/gabrielbelli/jailmachine/internal/machine"
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
			var err error
			if JSON() {
				err = doctor.WriteJSON(stdout, rep)
			} else {
				err = doctor.WriteTable(stdout, rep)
			}
			if err != nil {
				return err
			}
			if _, _, fail := rep.Counts(); fail > 0 {
				return fmt.Errorf("doctor: %d check(s) failed", fail)
			}
			return nil
		},
	}
}

// machineChecks produces one result per machine directory under the state
// root: the record must load, its backend and provider must be known, and
// the combined state must not be broken (ADR 0005). Directories that
// Store.List would skip (unreadable records) are reported, not hidden.
func machineChecks(context.Context) []doctor.Result {
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
	}
	return out
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
