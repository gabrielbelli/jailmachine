// Package idle decides when a running machine has been idle long enough to
// be suspended (ADR 0009). It is pure: the sleeper takes the samples (a
// guest probe over the control channel, host client activity, hypervisor
// CPU) and a Tracker turns them into idle time.
package idle

import (
	"fmt"
	"time"
)

const (
	// ProbeInterval is how often the sleeper takes a sample.
	ProbeInterval = 15 * time.Second
	// BusyCPUPercent is the hypervisor CPU use, in percent of one core,
	// above which a sample is busy. To be recalibrated from an idle guest
	// (twice its p99, never below 10) before idle suspend is on by default.
	BusyCPUPercent = 25.0
	// FlapWindow, MaxPenalty and PenaltyResetAfter are the flap guard: a
	// wake that follows its suspend within FlapWindow doubles the idle
	// period, up to MaxPenalty times; a suspension that lasts
	// PenaltyResetAfter or longer puts it back.
	FlapWindow        = 10 * time.Minute
	MaxPenalty        = 8
	PenaltyResetAfter = time.Hour
	// consecutive is how many samples in a row an engine client or a
	// command session must be seen before it holds the machine awake: a
	// single one is jm's own short work (a pf reload, a resolver push).
	consecutive = 2
)

// Tracker accumulates idle time from samples. The zero value is ready to use
// with the package defaults.
type Tracker struct {
	// Interval is the sampling interval; zero means ProbeInterval. One
	// sample adds at most twice this much idle time, so a host that slept,
	// a stopped process or a stalled probe never counts down on its own.
	Interval time.Duration
	// BusyCPU is the busy threshold in percent of one core; zero means
	// BusyCPUPercent.
	BusyCPU float64

	idle        time.Duration
	engineRun   int
	sessionsRun int
	penalty     int
	blockers    []string
}

// Observe adds one sample taken elapsed after the previous one (measured on
// the monotonic clock). A sample is quiet only if every signal is: a quiet
// sample adds min(elapsed, 2×Interval); a blocker resets the idle time to
// zero. Jails, the inhibit file, host activity and a busy CPU block at once;
// an engine client beyond the baseline or a command session blocks once
// seen in two consecutive samples, and a first sighting neither counts nor
// resets. A probe error (err) neither counts nor resets, although host
// activity and CPU seen alongside it still reset.
func (t *Tracker) Observe(s Sample, err error, elapsed time.Duration) {
	var now []string
	if s.Activity {
		now = append(now, "a jm command used the machine")
	}
	if s.CPUKnown && s.CPUPercent > t.busyCPU() {
		now = append(now, fmt.Sprintf("guest CPU %.0f%%", s.CPUPercent))
	}
	if err != nil {
		t.blockers = append(now, "guest activity unreadable: "+err.Error())
		if len(now) > 0 {
			t.idle = 0
		}
		return
	}
	if b := jailsBlocker(s.Jails); b != "" {
		now = append(now, b)
	}
	if s.Inhibit {
		now = append(now, inhibitBlocker)
	}
	pending := false
	if n := s.Engine - s.EngineBaseline; n > 0 {
		t.engineRun++
		if t.engineRun >= consecutive {
			now = append(now, engineBlocker(n))
		} else {
			pending = true
		}
	} else {
		t.engineRun = 0
	}
	if s.Sessions > 0 {
		t.sessionsRun++
		if t.sessionsRun >= consecutive {
			now = append(now, sessionsBlocker(s.Sessions))
		} else {
			pending = true
		}
	} else {
		t.sessionsRun = 0
	}
	t.blockers = now
	switch {
	case len(now) > 0:
		t.idle = 0
	case pending:
	default:
		t.idle += min(max(elapsed, 0), 2*t.interval())
	}
}

// Idle is the idle time accumulated so far.
func (t *Tracker) Idle() time.Duration { return t.idle }

// Blockers is what held the machine awake in the last sample; nil when it
// was quiet.
func (t *Tracker) Blockers() []string { return append([]string(nil), t.blockers...) }

// Penalty is the flap guard's multiplier of the idle period, 1 to MaxPenalty.
func (t *Tracker) Penalty() int { return max(t.penalty, 1) }

// Threshold is how long the machine must be idle to be suspended, given the
// configured period: after × Penalty. Zero means never.
func (t *Tracker) Threshold(after time.Duration) time.Duration {
	if after <= 0 {
		return 0
	}
	return after * time.Duration(t.Penalty())
}

// Due reports whether the machine has been idle for Threshold(after). A zero
// period never fires and resets the idle time, so turning idle suspend on
// again starts from nothing.
func (t *Tracker) Due(after time.Duration) bool {
	if after <= 0 {
		t.Reset()
		return false
	}
	return t.idle >= t.Threshold(after)
}

// Reset sets the idle time back to zero: after a refused or failed suspend,
// or with idle suspend off. The consecutive-sample counts and the penalty
// are kept.
func (t *Tracker) Reset() { t.idle = 0 }

// Restart forgets everything but the penalty: the sleeper calls it whenever
// it starts monitoring (after boot, a wake or a rollback).
func (t *Tracker) Restart() {
	t.idle, t.engineRun, t.sessionsRun, t.blockers = 0, 0, 0, nil
}

// Woke applies the flap guard to a wake that came asleepFor after the
// suspend committed: within FlapWindow the penalty doubles (up to
// MaxPenalty); after PenaltyResetAfter or more it goes back to 1.
func (t *Tracker) Woke(asleepFor time.Duration) {
	switch {
	case asleepFor < FlapWindow:
		t.penalty = min(t.Penalty()*2, MaxPenalty)
	case asleepFor >= PenaltyResetAfter:
		t.penalty = 1
	}
}

func (t *Tracker) interval() time.Duration {
	if t.Interval > 0 {
		return t.Interval
	}
	return ProbeInterval
}

func (t *Tracker) busyCPU() float64 {
	if t.BusyCPU > 0 {
		return t.BusyCPU
	}
	return BusyCPUPercent
}
