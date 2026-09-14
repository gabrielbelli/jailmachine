package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// remountTimeout bounds the guest step that mounts shares a suspend
// unmounted.
const remountTimeout = 30 * time.Second

// recoverInterruptedTransition resolves a suspend or wake that a killed jm
// left half done (ADR 0009 recovery table). It runs first under the lock in
// every command that changes a machine's lifecycle, before the state is
// computed, so that the command then sees running, suspended or stopped. The
// backend does the hypervisor half and never continues a guest whose saved
// state is committed; the provider is parked here when the machine ends up
// suspended or stopped, so a network left up by an interrupted wake does not
// make it read broken.
//
// A guest that was continued needs its shares remounted and its helpers
// restarted; the running stages of "jm start" do both (remountSharesIfPending,
// the connect and forwarder stages), so the action is only reported here.
func recoverInterruptedTransition(ctx context.Context, m *machine.Machine, b backend.Backend, p netprov.Provider) (backend.RecoverAction, error) {
	s, ok := b.(backend.Suspender)
	if !ok || m.Dir == "" {
		return backend.RecoverNone, nil
	}
	action, err := s.Recover(ctx, m)
	if err != nil {
		return action, withHint(fmt.Errorf("resolving an interrupted suspend or wake of %s: %w", m.Name, err), consoleHint(m, b))
	}
	switch action {
	case backend.RecoverSuspended, backend.RecoverDiscarded:
		if err := parkProvider(ctx, m, p); err != nil {
			fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
		}
		// The resolver exists only to answer a running guest, and it dials
		// the SSH port; its port stays recorded for the next wake.
		stopResolver(ctx, m)
	}
	switch action {
	case backend.RecoverResumedGuest:
		logf(stdout, "recovered %s: an interrupted suspend or wake was rolled back; the guest is running", m.Name)
	case backend.RecoverDiscarded:
		logf(stdout, "recovered %s: an incomplete saved state was discarded; the machine is stopped", m.Name)
	}
	return action, nil
}

// parkProvider stops the provider without taking the host engine socket from
// whoever serves it (netprov.Parker); a provider without that distinction is
// stopped.
func parkProvider(ctx context.Context, m *machine.Machine, p netprov.Provider) error {
	if pk, ok := p.(netprov.Parker); ok {
		return pk.Park(ctx, m)
	}
	return p.Stop(ctx, m)
}

// jmRemountFn defines the guest shell function jm_remount, which mounts the
// shares a suspend unmounted (ADR 0009). It first runs the boot-time service,
// which mounts the configuration share and every share in the table that is
// not mounted yet, and then every line of the list the suspend saved that is
// still not mounted, in the saved order: mount -p lists parents first. A
// line whose tag is no longer in the share table is skipped: that share's
// host path vanished while the machine was suspended, and its device only
// carries an empty placeholder (ADR 0007). It is idempotent and never removes
// the list; that is the caller's job, once the function has succeeded.
//
// Saved paths keep mount -p's octal escapes (a space is \040). They reach awk
// through the environment, never awk -v, which would decode the escapes and
// never match the escaped mount -p column again.
const jmRemountFn = `jm_remount() {
  if [ -x /usr/local/etc/rc.d/jm_shares ]; then
    service jm_shares start >/dev/null 2>&1 || true
  fi
  [ -f ` + machine.GuestSuspendMounts + ` ] || return 0
  _rc=0
  while read -r _tag _dir _fs _opts _rest; do
    [ -n "$_dir" ] || continue
    if [ "$_tag" != ` + machine.GuestConfTag + ` ] && [ -f ` + machine.GuestSharesTab + ` ]; then
      JM_T="$_tag" awk '$1 == ENVIRON["JM_T"] { f = 1 } END { exit !f }' ` + machine.GuestSharesTab + ` || continue
    fi
    mount -p -t p9fs | JM_D="$_dir" awk '$2 == ENVIRON["JM_D"] { f = 1 } END { exit !f }' && continue
    _d=$(printf '%b' "$_dir")
    mkdir -p "$_d" 2>/dev/null
    mount -t p9fs -o "$_opts" "$_tag" "$_d" || { echo "remount failed: $_d"; _rc=1; }
  done < ` + machine.GuestSuspendMounts + `
  return $_rc
}
`

// remountScript mounts the shares a suspend left unmounted and removes the
// list once every one of them is back. A guest with no list does nothing.
func remountScript() string {
	return jmRemountFn +
		"if [ -f " + machine.GuestSuspendMounts + " ]; then\n" +
		"  jm_remount && rm -f " + machine.GuestSuspendMounts + " && echo remounted\n" +
		"fi\n"
}

// remountSharesIfPending is the running-path half of the share remount: a
// guest continued by recovery, or a wake interrupted between continuing the
// guest and its post-resume script, still has its shares unmounted, and the
// next ordinary start mounts them. A failure is a warning; the list stays for
// the next start.
func remountSharesIfPending(ctx context.Context, m *machine.Machine, client *sshx.Client) {
	ctx, cancel := context.WithTimeout(ctx, remountTimeout)
	defer cancel()
	out, errOut, err := client.RunScript(ctx, "mounting the shares a suspend unmounted", remountScript())
	if err != nil {
		if msg := strings.TrimSpace(errOut + "\n" + out); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		fmt.Fprintf(stderr, "jm: warning: %s: shares unmounted by a suspend are not all back (retried on the next start): %v\n", m.Name, err)
		return
	}
	if strings.Contains(out, "remounted") {
		logf(stdout, "%s: mounted the shares an interrupted suspend or wake left unmounted", machine.StageSSH)
	}
}
