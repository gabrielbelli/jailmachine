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
}

func newDebouncer(delay time.Duration) *debouncer {
	c := make(chan struct{}, 1)
	return &debouncer{C: c, c: c, delay: delay}
}

// Trigger (re)starts the delay; the signal fires once it lapses without
// another Trigger. A signal nobody has consumed yet is not duplicated.
func (d *debouncer) Trigger() {
	d.mu.Lock()
	defer d.mu.Unlock()
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
