// Package forwarder publishes container ports on the host by reconciliation
// (ADR 0004): it derives the desired set of host<->guest mappings from the
// guest engine ("podman ps"), converges the network provider's mapping table
// to it, and repeats on container events, on a timer and on reconnect.
//
// The guest stays unaware of the host: podman in the guest publishes
// "-p 8080:80" on the guest's own address, and the forwarder maps
// <host_ip>:8080 on the host to <guest_ip>:8080.
//
// The forwarder only ever removes mappings it created itself. The set it
// owns is persisted in forwards.json (atomically) so that a restarted
// forwarder never unexposes the SSH port or mappings added by hand, and so
// that "jm inspect"/"jm ports" can show the table and per-mapping errors
// without talking to the provider.
package forwarder

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// Files the forwarder keeps in the machine directory.
const (
	StateFile = "forwards.json"
	PIDFile   = "forwarder.pid"
	LogFile   = "forwarder.log"
)

// Loop timings (defaults; Config overrides them for tests).
const (
	DefaultResync     = 30 * time.Second
	DefaultDebounce   = 300 * time.Millisecond
	DefaultMinBackoff = 1 * time.Second
	DefaultMaxBackoff = 10 * time.Second
)

// StatePath returns the path of forwards.json for a machine directory.
func StatePath(dir string) string { return filepath.Join(dir, StateFile) }

// Engine is the guest container engine as seen from the host: a container
// listing and an event stream. The CLI implements it with the host podman
// client; tests fake it.
type Engine interface {
	// PS returns the JSON of "podman ps --format json" (running containers).
	PS(ctx context.Context) ([]byte, error)
	// Events opens "podman events --format json --filter type=container";
	// the stream is one JSON object per line and stays open until ctx is
	// cancelled or the connection drops.
	Events(ctx context.Context) (io.ReadCloser, error)
}

// Config wires one forwarder run.
type Config struct {
	Provider netprov.Provider
	Machine  *machine.Machine
	// GuestIP is the address the guest publishes on (Endpoint.GuestIP).
	GuestIP string
	// HostIP is the host address published ports bind to (the machine's
	// PublishAddr); empty means DefaultHostIP.
	HostIP string
	Engine Engine
	// StatePath is where forwards.json lives (StatePath(m.Dir) normally).
	StatePath string
	// SSHLocal is the host side of the provider's own SSH forward
	// ("host:port"), which is never ours; "" if unknown.
	SSHLocal string
	Log      *log.Logger

	Resync     time.Duration
	Debounce   time.Duration
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func (c *Config) defaults() {
	c.HostIP = HostIP(c.HostIP)
	if c.Resync <= 0 {
		c.Resync = DefaultResync
	}
	if c.Debounce <= 0 {
		c.Debounce = DefaultDebounce
	}
	if c.MinBackoff <= 0 {
		c.MinBackoff = DefaultMinBackoff
	}
	if c.MaxBackoff < c.MinBackoff {
		c.MaxBackoff = DefaultMaxBackoff
	}
	if c.Log == nil {
		c.Log = log.New(io.Discard, "", 0)
	}
}

// relevant lists the container event actions that change the published
// port set (podman calls the action "Status").
var relevant = map[string]bool{
	"start": true, "died": true, "stop": true, "remove": true,
	"restart": true, "pause": true, "unpause": true,
}

// event is the subset of a podman event we look at.
type event struct {
	Type   string `json:"Type"`
	Status string `json:"Status"`
	Name   string `json:"Name"`
}

// Relevant reports whether a JSON event line should trigger a resync.
func Relevant(line []byte) bool {
	var e event
	if err := json.Unmarshal(line, &e); err != nil {
		return false
	}
	if e.Type != "" && e.Type != "container" {
		return false
	}
	return relevant[e.Status]
}

// Leaked returns the mappings in live that point at guestIP apart from the
// provider's own SSH forward (local sshLocal, or remote port 22): with an
// empty owned set these can only be leftovers of a forwarder whose state
// was lost.
func Leaked(live []netprov.Mapping, guestIP, sshLocal string) []netprov.Mapping {
	var out []netprov.Mapping
	for _, mp := range live {
		host, port, err := net.SplitHostPort(mp.Remote)
		if err != nil || host != guestIP {
			continue
		}
		if port == "22" || (sshLocal != "" && mp.Local == sshLocal) {
			continue
		}
		out = append(out, mp)
	}
	return out
}

// Run executes the reconciliation loop until ctx is cancelled: a full
// resync first, then one on each relevant event (debounced) and every
// Resync. The event stream is reopened with exponential backoff when it
// drops. Run returns nil on cancellation; it returns an error only when the
// provider has no mapping API at all (nothing to do).
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	if cfg.GuestIP == "" {
		return errors.New("forwarder: provider gives the guest no address reachable from the host")
	}
	st, err := Load(cfg.StatePath)
	if err != nil {
		cfg.Log.Printf("state: %v; starting with an empty owned set", err)
		st = &State{}
	}
	if len(st.Owned) == 0 {
		// Nothing on record, yet the provider may still hold mappings to
		// the guest from a forwarder whose state was lost. They are not
		// adopted (ADR 0004: only remove what we created); say so.
		if live, err := cfg.Provider.List(ctx, cfg.Machine); err == nil {
			if leaked := Leaked(live, cfg.GuestIP, cfg.SSHLocal); len(leaked) > 0 {
				cfg.Log.Printf("state: %s has no record of %v, which point at the guest; not adopting them (jm stop && jm start resets them)", cfg.StatePath, leaked)
			}
		}
	}

	resync := func(why string) error {
		data, err := cfg.Engine.PS(ctx)
		if err != nil {
			cfg.Log.Printf("resync (%s): listing containers: %v", why, err)
			return nil
		}
		desired, skipped, err := Plan(data, cfg.GuestIP, cfg.HostIP)
		if err != nil {
			cfg.Log.Printf("resync (%s): %v", why, err)
			return nil
		}
		res, err := ConvergeWith(ctx, cfg.Provider, cfg.Machine, desired, skipped, st, cfg.StatePath)
		if errors.Is(err, netprov.ErrUnsupported) {
			return err
		}
		if err != nil {
			cfg.Log.Printf("resync (%s): %v", why, err)
			return nil
		}
		if res.changed() {
			cfg.Log.Printf("resync (%s): %s", why, res)
		}
		return nil
	}
	if err := resync("start"); err != nil {
		return err
	}

	deb := newDebouncer(cfg.Debounce)
	defer deb.stop()
	go streamEvents(ctx, cfg, deb)

	ticker := time.NewTicker(cfg.Resync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := resync("timer"); err != nil {
				return err
			}
		case <-deb.C:
			if err := resync("event"); err != nil {
				return err
			}
		}
	}
}

// streamEvents keeps the event stream open, triggering the debouncer on
// relevant events and reconnecting with backoff on errors.
func streamEvents(ctx context.Context, cfg Config, deb *debouncer) {
	backoff := cfg.MinBackoff
	for ctx.Err() == nil {
		rc, err := cfg.Engine.Events(ctx)
		if err == nil {
			err = readEvents(rc, deb, &backoff, cfg.MinBackoff)
			_ = rc.Close()
		}
		if ctx.Err() != nil {
			return
		}
		cfg.Log.Printf("event stream: %v; reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
}

// readEvents consumes lines until the stream ends; it resets the backoff
// once a line has been read (the connection worked). It also triggers a
// resync when the stream closes, since events may have been missed.
func readEvents(r io.Reader, deb *debouncer, backoff *time.Duration, min time.Duration) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		*backoff = min
		if Relevant(sc.Bytes()) {
			deb.Trigger()
		}
	}
	deb.Trigger()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading: %w", err)
	}
	return errors.New("stream closed")
}
