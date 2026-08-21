package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// The guest clock. A virtual machine's timekeeping stops with the host: when
// the Mac sleeps, the guest wakes minutes or hours behind, and everything
// that cares about time — TLS certificates, build timestamps, package
// signatures, "podman ps" ages — is wrong until something corrects it.
//
// The correction is the guest's own: jm-rtcsync (installed by
// guest/provision.sh) reads the hypervisor's RTC, which tracks host wall
// time, and steps the system clock whenever the two diverge. Nothing has to
// be pushed from the host and nothing depends on the guest reaching an NTP
// server.
//
// What is left for the host is a single check at start, below: it costs one
// ssh round trip, corrects a machine whose guest predates the service, and
// gives "jm doctor" a way to say the clock is right.
const (
	// ClockSkewThreshold is how far the guest clock may be from the host's
	// before jm steps it. It matches jm-rtcsync's own threshold and is
	// well clear of the ssh round trip the measurement costs.
	ClockSkewThreshold = 5 * time.Second
	// clockTimeout bounds the guest commands, so an unresponsive machine
	// delays start by seconds rather than stalling it.
	clockTimeout = 20 * time.Second
	// guestClockCmd prints the guest's idea of the time as a Unix
	// timestamp, plus whether the resync service is running.
	guestClockCmd = "date -u +%s; service jm_rtcsync status >/dev/null 2>&1 && echo rtcsync=running || echo rtcsync=absent"
)

// GuestClock is what the guest answered: its clock, how far it is from the
// host's (positive when the guest is ahead) and whether the resync service
// is running there.
type GuestClock struct {
	Skew    time.Duration
	Service bool
}

// readGuestClock measures the guest's clock against the host's. The result
// is accurate to about a second: the guest's date(1) has no sub-second
// output, which is an order of magnitude finer than the skew that matters.
func readGuestClock(ctx context.Context, client *sshx.Client) (GuestClock, error) {
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	out, _, err := client.Run(ctx, guestClockCmd)
	if err != nil {
		return GuestClock{}, fmt.Errorf("reading the guest clock: %w", err)
	}
	return parseGuestClock(out, time.Now())
}

// parseGuestClock turns the output of guestClockCmd into a GuestClock,
// given the host time the command was run at. It is separate from the ssh
// call so it can be unit-tested.
func parseGuestClock(out string, hostNow time.Time) (GuestClock, error) {
	var gc GuestClock
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return gc, fmt.Errorf("guest clock: no output from %q", guestClockCmd)
	}
	secs, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return gc, fmt.Errorf("guest clock: unexpected output %q", strings.TrimSpace(out))
	}
	gc.Skew = time.Unix(secs, 0).Sub(hostNow).Truncate(time.Second)
	for _, f := range fields[1:] {
		if f == "rtcsync=running" {
			gc.Service = true
		}
	}
	return gc, nil
}

// stepGuestClock sets the guest clock to the host's. It is only reached
// when the two are already seconds apart, so the round trip's own error
// does not matter.
func stepGuestClock(ctx context.Context, client *sshx.Client, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	// date(1) sets the clock from a [[[[[cc]yy]mm]dd]HH]MM[.ss] string; -u
	// makes it UTC, which is what the host time is converted to.
	stamp := now.UTC().Format("200601021504.05")
	if out, _, err := client.Run(ctx, "date -u "+stamp); err != nil {
		return fmt.Errorf("setting the guest clock: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// syncGuestClock is the clock check "jm start" runs once sshd answers: it
// steps the guest clock when it has drifted (a host sleep, a guest without
// the resync service) and says so. A guest that cannot be asked is not a
// reason to fail a start, so every problem here is a warning.
func syncGuestClock(ctx context.Context, m *machine.Machine, client *sshx.Client) {
	gc, err := readGuestClock(ctx, client)
	if err != nil {
		fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
		return
	}
	if abs(gc.Skew) < ClockSkewThreshold {
		return
	}
	logf(stdout, "%s: guest clock is %s the host's; correcting it", machine.StageSSH, skewWord(gc.Skew))
	if err := stepGuestClock(ctx, client, time.Now()); err != nil {
		fmt.Fprintf(stderr, "jm: %v; continuing\n", err)
		return
	}
	if !gc.Service {
		fmt.Fprintf(stderr, "jm: %s has no jm-rtcsync service, so its clock will drift again after the host sleeps; re-create the machine to get one\n", m.Name)
	}
}

// skewWord renders a skew the way a person would say it, as the first half
// of "... the host" / "... the host's clock".
func skewWord(d time.Duration) string {
	switch {
	case abs(d) < time.Second:
		return "in step with"
	case d < 0:
		return abs(d).String() + " behind"
	default:
		return d.String() + " ahead of"
	}
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
