package idle

import (
	"errors"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// runAwk runs one of the probe's awk programs over a fixture with the host's
// awk, which shares the one true awk's dialect with FreeBSD's.
func runAwk(t *testing.T, prog, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("awk", append(args, prog)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("awk: %v", err)
	}
	return string(out)
}

const quiet = "v=1\njails=0\nengine=0\nsessions=0\nsshd=1\ninhibit=0\n"

func TestParseProbe(t *testing.T) {
	// sockstat -u: an ssh -L channel to the podman socket counts; the
	// podman service's own listener and an unrelated sshd socket do not.
	sockstat := `USER     COMMAND    PID   FD  PROTO  LOCAL ADDRESS         FOREIGN ADDRESS
root     sshd-sessi 4242  5   stream ?? -> /var/run/podman/po
root     podman     900   6   stream /var/run/podman/podma
root     sshd-sessi 4300  7   stream ?? -> /var/run/logpriv
`
	if got := runAwk(t, engineAwk, sockstat); strings.Count(got, "\n") != 1 || !strings.Contains(got, "4242") {
		t.Errorf("engine lines = %q", got)
	}

	// ps -ax -o pid= -o ppid= -o comm=: the tunnel's session has no child;
	// a command session's sleep counts; the probe's own shell ($$) and its
	// ps and awk (children of that shell) do not.
	ps := `    1     0 init
  500     1 sshd
  600   500 sshd-session
  601   600 sshd-session
  700   500 sshd-session
  701   700 sshd-session
  702   701 sleep
  800   500 sshd-session
  801   800 sshd-session
  802   801 sh
  803   802 ps
  804   802 awk
`
	got := runAwk(t, sessionsAwk, ps, "-v", "self=802")
	if got != "sessions=1\nsshd=6\n" {
		t.Errorf("sessions output = %q", got)
	}

	s, err := ParseProbe("v=1\njails=2\nengine=1\nsessions=3\nsshd=4\ninhibit=1\n")
	if err != nil || s.Jails != 2 || s.Engine != 1 || s.Sessions != 3 || s.SSHD != 4 || !s.Inhibit {
		t.Fatalf("parse = %+v, %v", s, err)
	}
	s.EngineBaseline = 1
	if got := s.Blockers(); !slices.Equal(got, []string{"2 jails or containers running", "3 command sessions open", machine.GuestNoSleep + " exists"}) {
		t.Errorf("blockers with the forwarder's stream = %q", got)
	}
	s.EngineBaseline = 0
	if got := s.Blockers(); len(got) != 4 || got[1] != "1 engine client connected" {
		t.Errorf("blockers without it = %q", got)
	}
	if _, err := ParseProbe(quiet); err != nil {
		t.Errorf("quiet probe: %v", err)
	}
	for name, bad := range map[string]string{
		"sshd=0":      strings.Replace(quiet, "sshd=1", "sshd=0", 1),
		"missing key": strings.Replace(quiet, "sessions=0\n", "", 1),
		"no version":  strings.Replace(quiet, "v=1\n", "", 1),
		"version 2":   strings.Replace(quiet, "v=1", "v=2", 1),
		"garbled":     strings.Replace(quiet, "jails=0", "jails=x", 1),
		"negative":    strings.Replace(quiet, "jails=0", "jails=-1", 1),
		"stray line":  quiet + "sh: jls: not found\n",
		"empty":       "",
	} {
		if _, err := ParseProbe(bad); err == nil {
			t.Errorf("%s: parse(%q) should fail", name, bad)
		}
	}
}

func TestProbeScriptPsSyntax(t *testing.T) {
	if !strings.Contains(ProbeScript, "ps -ax -o pid= -o ppid= -o comm=") {
		t.Errorf("the probe must give each ps keyword its own -o:\n%s", ProbeScript)
	}
	// FreeBSD's ps takes "pid=,ppid=" as one header: never join keywords.
	if regexp.MustCompile(`-o [a-z]+=,`).MatchString(ProbeScript) {
		t.Errorf("the probe joins ps keywords after '=':\n%s", ProbeScript)
	}
	for _, key := range probeKeys {
		if !strings.Contains(ProbeScript, key+"=") {
			t.Errorf("the probe never prints %s", key)
		}
	}
	if !strings.Contains(ProbeScript, "self=$$") || !strings.Contains(ProbeScript, machine.GuestNoSleep) {
		t.Errorf("probe:\n%s", ProbeScript)
	}
	if strings.Contains(sessionsAwk, "'") || strings.Contains(engineAwk, "'") {
		t.Error("an awk program contains a single quote, which ends its shell quoting")
	}
	if out, err := exec.Command("/bin/sh", "-n", "-c", ProbeScript).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v: %s", err, out)
	}
}

func mustParse(t *testing.T, out string) Sample {
	t.Helper()
	s, err := ParseProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTrackerQuietAccumulatesCapped(t *testing.T) {
	var tr Tracker
	q := mustParse(t, quiet)
	tr.Observe(q, nil, 15*time.Second)
	tr.Observe(q, nil, 20*time.Second)
	if tr.Idle() != 35*time.Second {
		t.Errorf("idle = %s", tr.Idle())
	}
	// A host that slept for eight hours adds one capped sample.
	tr.Observe(q, nil, 8*time.Hour)
	if tr.Idle() != 65*time.Second {
		t.Errorf("idle after a long gap = %s, want 65s", tr.Idle())
	}
	tr.Observe(q, nil, -time.Second)
	if tr.Idle() != 65*time.Second {
		t.Errorf("a negative elapsed changed idle: %s", tr.Idle())
	}
	short := Tracker{Interval: time.Second}
	short.Observe(q, nil, time.Minute)
	if short.Idle() != 2*time.Second {
		t.Errorf("cap with a 1 s interval = %s", short.Idle())
	}
	if tr.Blockers() != nil {
		t.Errorf("quiet blockers = %q", tr.Blockers())
	}
}

func TestTrackerTwoConsecutiveRule(t *testing.T) {
	for _, tc := range []struct {
		name, busy, want string
	}{
		{"engine", strings.Replace(quiet, "engine=0", "engine=2", 1), "1 engine client connected"},
		{"sessions", strings.Replace(quiet, "sessions=0", "sessions=1", 1), "1 command session open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr Tracker
			q, b := mustParse(t, quiet), mustParse(t, tc.busy)
			b.EngineBaseline = 1
			tr.Observe(q, nil, 15*time.Second)
			// A first sighting holds: no idle time, no reset.
			tr.Observe(b, nil, 15*time.Second)
			if tr.Idle() != 15*time.Second || tr.Blockers() != nil {
				t.Fatalf("after one busy sample: idle %s, blockers %q", tr.Idle(), tr.Blockers())
			}
			// A quiet sample in between breaks the run.
			tr.Observe(q, nil, 15*time.Second)
			tr.Observe(b, nil, 15*time.Second)
			if tr.Idle() != 30*time.Second {
				t.Fatalf("interrupted run reset idle: %s", tr.Idle())
			}
			// Two in a row reset.
			tr.Observe(b, nil, 15*time.Second)
			if tr.Idle() != 0 || !slices.Equal(tr.Blockers(), []string{tc.want}) {
				t.Errorf("after two busy samples: idle %s, blockers %q", tr.Idle(), tr.Blockers())
			}
		})
	}
	// The forwarder's own stream is the baseline, never a client.
	var tr Tracker
	s := mustParse(t, strings.Replace(quiet, "engine=0", "engine=1", 1))
	s.EngineBaseline = 1
	for range 3 {
		tr.Observe(s, nil, 15*time.Second)
	}
	if tr.Idle() != 45*time.Second {
		t.Errorf("baseline engine stream held the machine: idle %s", tr.Idle())
	}
}

func TestTrackerJailsAndActivityResetImmediately(t *testing.T) {
	q := mustParse(t, quiet)
	for name, busy := range map[string]Sample{
		"jails":    mustParse(t, strings.Replace(quiet, "jails=0", "jails=1", 1)),
		"inhibit":  mustParse(t, strings.Replace(quiet, "inhibit=0", "inhibit=1", 1)),
		"activity": {Activity: true, SSHD: 1},
	} {
		var tr Tracker
		tr.Observe(q, nil, 15*time.Second)
		tr.Observe(busy, nil, 15*time.Second)
		if tr.Idle() != 0 || len(tr.Blockers()) != 1 {
			t.Errorf("%s: idle %s, blockers %q", name, tr.Idle(), tr.Blockers())
		}
	}
}

func TestTrackerErrorNeitherCountsNorResets(t *testing.T) {
	var tr Tracker
	q := mustParse(t, quiet)
	tr.Observe(q, nil, 15*time.Second)
	b := mustParse(t, strings.Replace(quiet, "sessions=0", "sessions=1", 1))
	tr.Observe(b, nil, 15*time.Second) // first sighting
	probeErr := errors.New("ssh: connection reset")
	tr.Observe(Sample{}, probeErr, 15*time.Second)
	tr.Observe(Sample{}, probeErr, 15*time.Second)
	if tr.Idle() != 15*time.Second {
		t.Errorf("idle after errors = %s", tr.Idle())
	}
	if bl := tr.Blockers(); len(bl) != 1 || !strings.Contains(bl[0], "connection reset") {
		t.Errorf("blockers after an error = %q", bl)
	}
	// The error did not break the run: the next busy sample is the second.
	tr.Observe(b, nil, 15*time.Second)
	if tr.Idle() != 0 {
		t.Errorf("a run across a probe error did not reset: %s", tr.Idle())
	}
	// Host activity alongside a probe error still resets.
	tr.Observe(q, nil, 15*time.Second)
	tr.Observe(Sample{Activity: true}, probeErr, 15*time.Second)
	if tr.Idle() != 0 {
		t.Errorf("host activity with a probe error did not reset: %s", tr.Idle())
	}
}

func TestTrackerThresholdZeroNeverFires(t *testing.T) {
	var tr Tracker
	q := mustParse(t, quiet)
	for range 100 {
		tr.Observe(q, nil, 30*time.Second)
	}
	if tr.Due(0) || tr.Idle() != 0 {
		t.Errorf("a zero period fired or kept idle time: %s", tr.Idle())
	}
	for range 10 {
		tr.Observe(q, nil, 30*time.Second)
	}
	if !tr.Due(5*time.Minute) || tr.Due(6*time.Minute) {
		t.Errorf("due with 5 min idle: 5 min %v, 6 min %v", tr.Due(5*time.Minute), tr.Due(6*time.Minute))
	}
	if tr.Threshold(0) != 0 {
		t.Error("threshold of a zero period")
	}
}

func TestFlapPenalty(t *testing.T) {
	var tr Tracker
	if tr.Penalty() != 1 {
		t.Fatalf("initial penalty = %d", tr.Penalty())
	}
	tr.Woke(3 * time.Minute)
	if tr.Penalty() != 2 || tr.Threshold(30*time.Minute) != time.Hour {
		t.Errorf("after one flap: penalty %d, threshold %s", tr.Penalty(), tr.Threshold(30*time.Minute))
	}
	for range 5 {
		tr.Woke(time.Minute)
	}
	if tr.Penalty() != MaxPenalty {
		t.Errorf("penalty cap = %d", tr.Penalty())
	}
	// A client that arrived during the suspend itself is a flap too.
	tr.Woke(-time.Second)
	if tr.Penalty() != MaxPenalty {
		t.Errorf("penalty after a negative wake = %d", tr.Penalty())
	}
	tr.Woke(30 * time.Minute) // between the window and the reset: unchanged
	if tr.Penalty() != MaxPenalty {
		t.Errorf("penalty after a 30 min sleep = %d", tr.Penalty())
	}
	tr.Restart()
	if tr.Penalty() != MaxPenalty {
		t.Error("Restart forgot the penalty")
	}
	tr.Woke(PenaltyResetAfter)
	if tr.Penalty() != 1 {
		t.Errorf("penalty after an hour asleep = %d", tr.Penalty())
	}
}

func TestCPUBlocker(t *testing.T) {
	pct := CPUPercent(7*time.Second, 15*time.Second)
	if pct < 46 || pct > 47 {
		t.Fatalf("7 s of 15 s = %.2f%%", pct)
	}
	var tr Tracker
	q := mustParse(t, quiet)
	tr.Observe(q, nil, 15*time.Second)
	busy := q
	busy.CPUPercent, busy.CPUKnown = pct, true
	tr.Observe(busy, nil, 15*time.Second)
	if tr.Idle() != 0 || !slices.Equal(tr.Blockers(), []string{"guest CPU 47%"}) {
		t.Errorf("idle %s, blockers %q", tr.Idle(), tr.Blockers())
	}
	calm := q
	calm.CPUPercent, calm.CPUKnown = CPUPercent(time.Second, 15*time.Second), true
	tr.Observe(calm, nil, 15*time.Second)
	if tr.Idle() != 15*time.Second {
		t.Errorf("7%% CPU was busy: idle %s", tr.Idle())
	}
	// An unknown CPU reading is not a blocker.
	unknown := q
	unknown.CPUPercent = 99
	tr.Observe(unknown, nil, 15*time.Second)
	if tr.Idle() != 30*time.Second {
		t.Errorf("an unmeasured CPU blocked: idle %s", tr.Idle())
	}
	if CPUPercent(time.Second, 0) != 0 {
		t.Error("zero elapsed")
	}
}

func TestParseCPUTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"  0:01.23\n": 1230 * time.Millisecond,
		"123:45.67":   123*time.Minute + 45670*time.Millisecond,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:02:03":  49*time.Hour + 2*time.Minute + 3*time.Second,
		"7.5":         7500 * time.Millisecond,
	} {
		got, err := ParseCPUTime(in)
		if err != nil || got != want {
			t.Errorf("ParseCPUTime(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "TIME", "1:2:3:4", "x:01", "-1:00", "1:-5", "1e3", "a-01:00"} {
		if _, err := ParseCPUTime(bad); err == nil {
			t.Errorf("ParseCPUTime(%q) should fail", bad)
		}
	}
}
