package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielbelli/jailmachine/internal/sleeper"
)

// newWakeCmd is the hidden "jm _wake <name>" the sleeper spawns, detached,
// when a connection arrives on a suspended machine's held endpoints (ADR
// 0009). It is a start that queues behind any command holding the lock,
// returns at once when that command already woke the machine, never folds
// its inherited environment into the record, and never boots a machine that
// was stopped meanwhile. It logs to wake.log.
func newWakeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    sleeper.WakeCommand + " [name]",
		Short:  "Wake a suspended machine for a held connection (internal)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := resolveName(args)
			if err != nil {
				return err
			}
			activeMachine = name
			began := time.Now()
			fmt.Fprintf(stdout, "%s _wake: waking %s for a held connection (pid %d)\n", began.Format(time.RFC3339), name, os.Getpid())
			err = startMachine(cmd.Context(), []string{name}, startOpts{waitLock: true, skipIfReady: true, fromHelper: true, wakeOnly: true})
			if err != nil {
				fmt.Fprintf(stdout, "%s _wake: %s: %v\n", time.Now().Format(time.RFC3339), name, err)
				return err
			}
			fmt.Fprintf(stdout, "%s _wake: %s is awake after %.1f s\n", time.Now().Format(time.RFC3339), name, time.Since(began).Seconds())
			return nil
		},
	}
}
