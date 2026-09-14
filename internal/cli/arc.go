package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// The guest's ZFS ARC. FreeBSD lets the ARC grow to nearly all of the
// guest's RAM when vfs.zfs.arc.max is unset, and QEMU keeps every page the
// guest has ever touched, so an idle machine's footprint on the host creeps
// towards its whole memory size over days (docs/LIMITATIONS.md). The cap is
// a machine setting, pushed over SSH at every start and by "jm set --arc" on
// a running machine: the runtime sysctl takes effect at once, and
// /boot/loader.conf keeps it across a guest reboot jm does not see.
const (
	// arcTimeout bounds the guest commands, as clockTimeout does.
	arcTimeout = 20 * time.Second
	// arcTunable is the sysctl and loader tunable the cap is written to.
	arcTunable = "vfs.zfs.arc.max"
	// guestLoaderConf is where the cap persists across guest reboots.
	guestLoaderConf = "/boot/loader.conf"
)

// validateArc range-checks an ARC cap against the machine's memory: 0 (the
// guest's default), or at least machine.MinArcMiB and below memoryMiB.
func validateArc(arcMiB, memoryMiB int) error {
	if arcMiB == 0 || (arcMiB >= machine.MinArcMiB && arcMiB < memoryMiB) {
		return nil
	}
	return fmt.Errorf("--arc must be 0 (the guest's default) or between %d MiB and %d MiB (below the memory)", machine.MinArcMiB, memoryMiB-1)
}

// arcWord renders an ARC cap for people: "512 MiB", or "guest default".
func arcWord(arcMiB int) string {
	if arcMiB == 0 {
		return "guest default"
	}
	return fmt.Sprintf("%d MiB", arcMiB)
}

// arcScript builds the guest shell program that applies an ARC cap.
func arcScript(arcMiB int) string { return arcScriptAt(arcMiB, guestLoaderConf) }

// arcScriptAt is arcScript with the loader.conf path as a parameter, so the
// program can be run against a scratch file in tests.
//
// The program is idempotent (nothing is written when the value is already
// in place) and quiet on success. The loader.conf half runs even when the
// runtime sysctl is refused, and the program then exits non-zero so the
// caller can warn. Three OpenZFS rules shape it:
//
//   - A runtime arc.max at or below the ARC's floor (kstat c_min, a 32nd of
//     the guest's memory unless tuned) is refused with EINVAL, so on a big
//     guest the floor is lowered first, to half the cap. At boot the kernel
//     does the same by itself for a loader.conf arc.max, so only arc.max is
//     persisted.
//   - A runtime arc.max of 0 is accepted and changes nothing: the old cap
//     stays until the guest reboots. Removing the cap therefore writes the
//     guest's default ceiling back (arc_default_max on FreeBSD: the larger
//     of 5/8 of the memory and the memory less 1 GiB).
//   - sysrc(8) refuses a name with dots in it, so loader.conf is edited with
//     grep into a temporary file that replaces the original only when grep
//     succeeded; a failed grep must never truncate loader.conf.
func arcScriptAt(arcMiB int, loaderConf string) string {
	var b strings.Builder
	pattern := `'^[[:space:]]*` + strings.ReplaceAll(arcTunable, ".", `\.`) + `[[:space:]]*='`
	fmt.Fprintf(&b, "rc=0\nconf=%s\n", shellQuote(loaderConf))
	b.WriteString("cmax=$(sysctl -n kstat.zfs.misc.arcstats.c_max 2>/dev/null) || cmax=\n")
	if arcMiB == 0 {
		b.WriteString("pm=$(sysctl -n hw.physmem 2>/dev/null) || pm=\n")
		b.WriteString("if [ -n \"$pm\" ]; then\n")
		b.WriteString("  def=$((pm * 5 / 8))\n")
		b.WriteString("  [ $((pm - 1073741824)) -gt \"$def\" ] && def=$((pm - 1073741824))\n")
		fmt.Fprintf(&b, "  [ \"$cmax\" = \"$def\" ] || sysctl %s=\"$def\" >/dev/null || rc=$?\n", arcTunable)
		b.WriteString("else\n  rc=1\nfi\n")
		fmt.Fprintf(&b, "if [ -f \"$conf\" ] && grep -q %s \"$conf\"; then\n", pattern)
		fmt.Fprintf(&b, "  grep -v %s \"$conf\" > \"$conf.jm\"\n", pattern)
		b.WriteString("  if [ $? -le 1 ] && mv \"$conf.jm\" \"$conf\"; then :; else rc=1; fi\n")
		b.WriteString("  rm -f \"$conf.jm\"\n")
		b.WriteString("fi\n")
	} else {
		bytes := int64(arcMiB) << 20
		floor := max(bytes/2, 32<<20)
		fmt.Fprintf(&b, "if [ \"$cmax\" != %d ]; then\n", bytes)
		b.WriteString("  cmin=$(sysctl -n kstat.zfs.misc.arcstats.c_min 2>/dev/null) || cmin=0\n")
		fmt.Fprintf(&b, "  if [ \"${cmin:-0}\" -ge %d ] 2>/dev/null; then sysctl vfs.zfs.arc.min=%d >/dev/null || rc=$?; fi\n", bytes, floor)
		fmt.Fprintf(&b, "  sysctl %s=%d >/dev/null || rc=$?\n", arcTunable, bytes)
		b.WriteString("fi\n")
		line := fmt.Sprintf(`%s="%d"`, arcTunable, bytes)
		b.WriteString("[ -f \"$conf\" ] || touch \"$conf\" || rc=$?\n")
		fmt.Fprintf(&b, "if ! grep -qxF '%s' \"$conf\" 2>/dev/null; then\n", line)
		fmt.Fprintf(&b, "  grep -v %s \"$conf\" > \"$conf.jm\"\n", pattern)
		fmt.Fprintf(&b, "  if [ $? -le 1 ] && echo '%s' >> \"$conf.jm\" && mv \"$conf.jm\" \"$conf\"; then :; else rc=1; fi\n", line)
		b.WriteString("  rm -f \"$conf.jm\"\n")
		b.WriteString("fi\n")
	}
	b.WriteString("exit $rc\n")
	return b.String()
}

// applyArc runs arcScript for m's cap on an existing connection.
func applyArc(ctx context.Context, m *machine.Machine, c *sshx.Client) error {
	ctx, cancel := context.WithTimeout(ctx, arcTimeout)
	defer cancel()
	out, errOut, err := c.RunScript(ctx, "setting the ZFS ARC cap", arcScript(m.ArcMiB))
	if err != nil {
		if msg := strings.TrimSpace(errOut + "\n" + out); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// applyArcLive is applyArc for "jm set --arc" on a running machine, which
// has no connection yet.
func applyArcLive(ctx context.Context, m *machine.Machine) error {
	ep, err := endpointOf(m)
	if err != nil {
		return err
	}
	c, err := sshx.Dial(ctx, ep.SSHHost, ep.SSHPort, m.SSHUser, sshKey(m))
	if err != nil {
		return err
	}
	defer c.Close()
	return applyArc(ctx, m, c)
}

// syncGuestArc is the ARC step "jm start" runs once sshd answers. A guest
// that refuses the cap is not a reason to fail a start, so a problem here
// is a warning, as it is for the clock.
func syncGuestArc(ctx context.Context, m *machine.Machine, client *sshx.Client) {
	if m.ArcMiB == 0 {
		logf(stdout, "%s: leaving the guest's ZFS ARC at its default size", machine.StageSSH)
	} else {
		logf(stdout, "%s: capping the guest's ZFS ARC at %d MiB", machine.StageSSH, m.ArcMiB)
	}
	if err := applyArc(ctx, m, client); err != nil {
		fmt.Fprintf(stderr, "jm: warning: ZFS ARC cap not applied: %v; continuing\n", err)
	}
}
