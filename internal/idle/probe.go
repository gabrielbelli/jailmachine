package idle

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// engineAwk keeps the sockstat -u lines of sshd processes connected to the
// guest podman socket: every ssh -L channel, podman-remote over ssh:// and
// the forwarder's events stream. sockstat truncates both the command
// ("sshd-sessi") and the peer path ("/var/run/podman/po"). The podman
// service's own listener is not an sshd line and is not counted.
const engineAwk = `$2 ~ /^sshd/ && /-> \/var\/run\/podman\/po/`

// sessionsAwk reads "pid ppid comm" lines and prints two keys: sessions, the
// processes whose parent is an sshd session and which are not sshd
// themselves, less the shell running the probe (self); and sshd, the number
// of sshd session processes. Every exec over ssh (jm ssh -- cmd, scp, a
// shell) is a child of an sshd-session; a tunnel's session has no child.
const sessionsAwk = `{ pid[NR] = $1; ppid[NR] = $2; comm[$1] = $3; n = NR }
END {
  s = 0; ssh = 0
  for (i = 1; i <= n; i++) {
    if (comm[pid[i]] ~ /^sshd-sess/) ssh++
    if (comm[ppid[i]] ~ /^sshd-sess/ && comm[pid[i]] !~ /^sshd/ && pid[i] != self) s++
  }
  printf "sessions=%d\nsshd=%d\n", s, ssh
}`

// ProbeVersion is the v= key ProbeScript prints; ParseProbe refuses others.
const ProbeVersion = 1

// ProbeScript is the guest half of one idle sample, run as one exec on the
// sleeper's control connection. FreeBSD's ps takes everything after "=" as
// the header, commas included, so each keyword has its own -o (measured
// 2026-09-14: -o pid=,ppid=,comm= prints the pid alone).
var ProbeScript = "printf 'v=" + strconv.Itoa(ProbeVersion) + `\n'
printf 'jails=%s\n' "$(jls jid | wc -l | tr -d ' ')"
printf 'engine=%s\n' "$(sockstat -u 2>/dev/null | awk '` + engineAwk + `' | wc -l | tr -d ' ')"
ps -ax -o pid= -o ppid= -o comm= | awk -v self=$$ '` + sessionsAwk + `'
printf 'inhibit=%s\n' "$([ -e ` + machine.GuestNoSleep + ` ] && echo 1 || echo 0)"
`

// probeKeys are the keys ParseProbe requires.
var probeKeys = []string{"v", "jails", "engine", "sessions", "sshd", "inhibit"}

// Sample is one observation of a running machine. The guest fields come from
// ProbeScript; the sleeper fills in the host ones.
type Sample struct {
	// Jails counts running jails: every container and bastille jail.
	Jails int
	// Engine counts sshd connections to the guest podman socket, and
	// EngineBaseline how many of those are jm's own (the forwarder's events
	// stream).
	Engine, EngineBaseline int
	// Sessions counts command sessions; SSHD the sshd session processes.
	Sessions, SSHD int
	// Inhibit is whether machine.GuestNoSleep exists.
	Inhibit bool

	// Activity is whether a host jm client used the machine since the
	// previous sample (the activity file's mtime changed).
	Activity bool
	// CPUPercent is the hypervisor's CPU use since the previous sample, in
	// percent of one core; CPUKnown says whether it could be measured.
	CPUPercent float64
	CPUKnown   bool
}

// ParseProbe reads ProbeScript's output. A missing or garbled key, another
// probe version, or no sshd session at all is an error: the probe runs in an
// sshd session itself, so a guest whose process titles changed fails as
// "never sleeps" rather than "always idle".
func ParseProbe(out string) (Sample, error) {
	vals := map[string]int{}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return Sample{}, fmt.Errorf("unexpected line %q in the idle probe", l)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Sample{}, fmt.Errorf("unexpected %q in the idle probe", l)
		}
		vals[k] = n
	}
	for _, k := range probeKeys {
		if _, ok := vals[k]; !ok {
			return Sample{}, fmt.Errorf("the idle probe did not report %s: %q", k, strings.TrimSpace(out))
		}
	}
	if vals["v"] != ProbeVersion {
		return Sample{}, fmt.Errorf("the idle probe reported version %d, want %d", vals["v"], ProbeVersion)
	}
	if vals["sshd"] == 0 {
		return Sample{}, fmt.Errorf("the idle probe saw no sshd session, not even its own; guest process titles have changed")
	}
	return Sample{
		Jails: vals["jails"], Engine: vals["engine"], Sessions: vals["sessions"],
		SSHD: vals["sshd"], Inhibit: vals["inhibit"] != 0,
	}, nil
}

// Blockers lists what in the guest keeps it from being suspended under the
// strict single-sample rules of a suspend about to happen: any jail, an
// engine client beyond the baseline, a command session, the inhibit file.
// Host signals are not included.
func (s Sample) Blockers() []string {
	var out []string
	if b := jailsBlocker(s.Jails); b != "" {
		out = append(out, b)
	}
	if b := engineBlocker(s.Engine - s.EngineBaseline); b != "" {
		out = append(out, b)
	}
	if b := sessionsBlocker(s.Sessions); b != "" {
		out = append(out, b)
	}
	if s.Inhibit {
		out = append(out, inhibitBlocker)
	}
	return out
}

var inhibitBlocker = machine.GuestNoSleep + " exists"

func jailsBlocker(n int) string {
	return count(n, "1 jail or container running", "%d jails or containers running")
}

func engineBlocker(n int) string {
	return count(n, "1 engine client connected", "%d engine clients connected")
}

func sessionsBlocker(n int) string {
	return count(n, "1 command session open", "%d command sessions open")
}

func count(n int, one, many string) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return one
	}
	return fmt.Sprintf(many, n)
}

// ParseCPUTime parses the cumulative CPU time ps prints for "-o time=":
// "0:01.23" and "123:45.67" (minutes and seconds, macOS), "01:02:03"
// (hours, minutes and seconds) and "2-01:02:03" (with days, Linux).
func ParseCPUTime(s string) (time.Duration, error) {
	orig := s
	s = strings.TrimSpace(s)
	bad := func() (time.Duration, error) { return 0, fmt.Errorf("unexpected cpu time %q", strings.TrimSpace(orig)) }
	if s == "" {
		return bad()
	}
	var total time.Duration
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, err := strconv.Atoi(d)
		if err != nil || days < 0 {
			return bad()
		}
		total += time.Duration(days) * 24 * time.Hour
		s = rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 1 || len(parts) > 3 {
		return bad()
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil || sec < 0 || strings.ContainsAny(parts[len(parts)-1], "eE+-") {
		return bad()
	}
	total += time.Duration(sec * float64(time.Second))
	units := []time.Duration{time.Minute, time.Hour}
	for i, j := len(parts)-2, 0; i >= 0; i, j = i-1, j+1 {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return bad()
		}
		total += time.Duration(n) * units[j]
	}
	return total, nil
}

// CPUPercent is cpu time used over elapsed wall time, in percent of one core.
func CPUPercent(cpu, elapsed time.Duration) float64 {
	if elapsed <= 0 || cpu < 0 {
		return 0
	}
	return float64(cpu) / float64(elapsed) * 100
}
