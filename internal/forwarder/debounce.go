package forwarder

import (
	"sync"
	"time"
)

// debouncer coalesces bursts of Trigger calls into one signal on C, sent
// delay after the last call. A "podman run" emits create/init/start within
// milliseconds; one resync covers them all.
type debouncer struct {
	C     <-chan struct{}
	c     chan struct{}
	delay time.Duration
	mu    sync.Mutex
	timer *time.Timer
	force bool
}

func newDebouncer(delay time.Duration) *debouncer {
	c := make(chan struct{}, 1)
	return &debouncer{C: c, c: c, delay: delay}
}

// Trigger (re)starts the delay; the signal fires once it lapses without
// another Trigger. A signal nobody has consumed yet is not duplicated.
func (d *debouncer) Trigger() { d.trigger(false) }

// TriggerForce is Trigger for a reason that also invalidates what the
// forwarder believes about state it cannot see — the guest's pf anchor
// after an event stream reconnect, which may span a guest reboot. The flag
// survives coalescing and is read once, by takeForce.
func (d *debouncer) TriggerForce() { d.trigger(true) }

// takeForce reports whether any trigger since the last call asked for a
// forced resync, and clears the flag.
func (d *debouncer) takeForce() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.force
	d.force = false
	return f
}

func (d *debouncer) trigger(force bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if force {
		d.force = true
	}
	if d.timer != nil {
		d.timer.Stop()
	}
	d.timer = time.AfterFunc(d.delay, func() {
		select {
		case d.c <- struct{}{}:
		default:
		}
	})
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
	}
}
