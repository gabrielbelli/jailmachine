package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// idleSuspendFlagUsage is the --idle-suspend help shared by init and set.
const idleSuspendFlagUsage = "suspend the machine to disk after this long idle (30m, 2h, 45 = minutes; 0 or off = never); it wakes on first use"

// ParseIdleSuspend parses an --idle-suspend value into minutes: 0, off or
// never (any case) is 0; otherwise a whole number of minutes, bare or with
// an m, min or h suffix (any case), between machine.MinIdleSuspendMin and
// machine.MaxIdleSuspendMin. Anything else is a usage error.
func ParseIdleSuspend(s string) (int, error) {
	bad := usagef("--idle-suspend must be 0 (never) or between %dm and %dh",
		machine.MinIdleSuspendMin, machine.MaxIdleSuspendMin/60)
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "off", "never":
		return 0, nil
	}
	num, mult := s, 1
	switch {
	case strings.HasSuffix(s, "min"):
		num = strings.TrimSuffix(s, "min")
	case strings.HasSuffix(s, "m"):
		num = strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "h"):
		num, mult = strings.TrimSuffix(s, "h"), 60
	}
	// Digits only: no sign, no fraction, no space before the suffix.
	if num == "" || strings.TrimLeft(num, "0123456789") != "" {
		return 0, bad
	}
	n, err := strconv.Atoi(num)
	if err != nil || n > machine.MaxIdleSuspendMin {
		return 0, bad
	}
	n *= mult
	if n != 0 && (n < machine.MinIdleSuspendMin || n > machine.MaxIdleSuspendMin) {
		return 0, bad
	}
	return n, nil
}

// idleSuspendWord renders an idle-suspend setting for people: "30 min", or
// "off".
func idleSuspendWord(mins int) string {
	if mins == 0 {
		return "off"
	}
	return fmt.Sprintf("%d min", mins)
}

// idleSuspendRow is the inspect row: "after 30 min", or "off".
func idleSuspendRow(mins int) string {
	if mins == 0 {
		return "off"
	}
	return "after " + idleSuspendWord(mins)
}
