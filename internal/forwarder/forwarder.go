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

// DefaultReloadEvery is how many timer resyncs pass before the guest's pf
// anchor is written again whether or not the rules changed. jm cannot see
// the guest's pf state, so an anchor that was correct when it was loaded
// can be emptied behind its back — a guest reboot with restart-policy
// containers coming back on the same addresses, "service pf restart",
// "pfctl -F nat" — and nothing in the desired state would ever change to
// make the forwarder notice. With the 30 s default resync this reloads it
// every five minutes, which bounds how long such a mapping can stay bound
// on the host and answer nothing.
const DefaultReloadEvery = 10

// StatePath returns the path of forwards.json for a machine directory.
func StatePath(dir string) string { return filepath.Join(dir, StateFile) }

// Engine is the guest container engine as seen from the host: a container
// listing, a container inspection and an event stream. The CLI implements
// it with the host podman client; tests fake it.
type Engine interface {
	// PS returns the JSON of "podman ps --format json" (running containers).
	PS(ctx context.Context) ([]byte, error)
	// Inspect returns the JSON of "podman inspect --type container <id>…"
	// for the given ids, which is the only place the container's address on
	// the guest's container network is written down. It is called only for
	// the containers whose publish needs a guest-side redirect, so the
	// common "-p 8080:80" costs no extra round trip.
	Inspect(ctx context.Context, ids []string) ([]byte, error)
	// Events opens "podman events --format json --filter type=container";
	// the stream is one JSON object per line and stays open until ctx is
	// cancelled or the connection drops.
	Events(ctx context.Context) (io.ReadCloser, error)
}

// Guest is jm's control channel into the guest, used for the half of
// publishing the engine cannot do: loading jm's own pf anchor with the
// redirects that make "-p <host address>:8080:80" reachable (see Rule).
// The CLI implements it over SSH; a nil Guest leaves those mappings bound
// on the host but not yet working, and says so per mapping.
type Guest interface {
	// ApplyRules loads text as the complete content of jm's anchor;
	// empty text flushes it.
	ApplyRules(ctx context.Context, text string) error
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
	// Guest applies jm's guest-side redirects; see Guest.
	Guest Guest
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
	// ReloadEvery is how many timer resyncs pass between unconditional
	// reloads of the guest anchor; 0 means DefaultReloadEvery.
	ReloadEvery int
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
	if c.ReloadEvery <= 0 {
		c.ReloadEvery = DefaultReloadEvery
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

	// The publish address the record carried when this forwarder started is
	// what it really binds until it is restarted, so "jm ports" and
	// "jm inspect" can tell a changed record from a running mapping table.
	if st.PublishAddr != cfg.HostIP {
		st.PublishAddr = cfg.HostIP
		if err := st.Save(cfg.StatePath); err != nil {
			cfg.Log.Printf("state: %v", err)
		}
	}

	var anchor anchorState
	// force asks apply to write the guest anchor whether or not the rules
	// changed: on start, on an event stream reconnect and every
	// ReloadEvery timer resyncs (see DefaultReloadEvery).
	resync := func(why string, force bool) error {
		data, err := cfg.Engine.PS(ctx)
		if err != nil {
			cfg.Log.Printf("resync (%s): listing containers: %v", why, err)
			return nil
		}
		pl, err := Compute(data, cfg.GuestIP, cfg.HostIP)
		if err != nil {
			cfg.Log.Printf("resync (%s): %v", why, err)
			return nil
		}
		anchor.apply(ctx, cfg, &pl, force)
		res, err := ConvergeWith(ctx, cfg.Provider, cfg.Machine, pl, st, cfg.StatePath)
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
	if err := resync("start", true); err != nil {
		return err
	}

	deb := newDebouncer(cfg.Debounce)
	defer deb.stop()
	go streamEvents(ctx, cfg, deb)

	ticker := time.NewTicker(cfg.Resync)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			ticks++
			if err := resync("timer", ticks%cfg.ReloadEvery == 0); err != nil {
				return err
			}
		case <-deb.C:
			if err := resync("event", deb.takeForce()); err != nil {
				return err
			}
		}
	}
}

// anchorState remembers the rule set last loaded into the guest, so an
// unchanged plan costs no SSH round trip. Until something has been loaded
// the state is unknown, and the first reconcile writes the anchor even when
// it is empty: that is what clears the rules of a forwarder that was killed
// while the guest kept running.
//
// The memo is a belief about the guest, not a reading of it, so it is only
// ever a fast path: apply's force argument bypasses it whenever the belief
// may be stale (a reconnect, every ReloadEvery timer resyncs), which is
// what heals an anchor the guest flushed on its own.
type anchorState struct {
	text   string
	loaded bool
}

// apply resolves the container addresses the plan's redirects need and
// loads them into jm's anchor in the guest. Every failure blocks the
// mappings that depend on it — their host leg is still bound, and the next
// reconcile retries — rather than dropping them, which would unpublish a
// working port because one "podman inspect" timed out.
func (a *anchorState) apply(ctx context.Context, cfg Config, pl *Plan, force bool) {
	if cfg.Guest == nil {
		if len(pl.Rules) > 0 {
			pl.Block("jm has no control channel to the guest to install the redirect this publish needs")
		}
		return
	}
	if len(pl.Rules) > 0 {
		// A failed lookup says nothing about the rules already loaded, so
		// the anchor is left as it is rather than emptied: tearing down a
		// working redirect because one inspect timed out would break a
		// published port for a whole resync interval.
		raw, err := cfg.Engine.Inspect(ctx, pl.ContainerIDs())
		if err != nil {
			pl.Block("looking up the container's address in the guest: " + err.Error())
			return
		}
		ips, err := ParseInspect(raw)
		if err != nil {
			pl.Block(err.Error())
			return
		}
		pl.Resolve(ips)
	}
	text := AnchorText(pl.Rules)
	if !force && a.loaded && text == a.text {
		return
	}
	if err := cfg.Guest.ApplyRules(ctx, text); err != nil {
		// The memo is now worthless either way: what the guest holds is
		// whatever pfctl left behind. Forget it so the next reconcile
		// writes the anchor again even if the rules did not change.
		a.loaded = false
		pl.Block("installing the redirect in the guest: " + err.Error())
		cfg.Log.Printf("guest redirects: %v", err)
		return
	}
	had := a.loaded && a.text != ""
	a.text, a.loaded = text, true
	switch {
	case len(pl.Rules) > 0:
		cfg.Log.Printf("guest redirects: loaded %d rule(s) into anchor %s: %v", len(pl.Rules), GuestAnchor, pl.Rules)
	case had:
		cfg.Log.Printf("guest redirects: anchor %s cleared", GuestAnchor)
	}
}

// streamEvents keeps the event stream open, triggering the debouncer on
// relevant events and reconnecting with backoff on errors.
func streamEvents(ctx context.Context, cfg Config, deb *debouncer) {
	backoff := cfg.MinBackoff
	reconnect := false
	for ctx.Err() == nil {
		rc, err := cfg.Engine.Events(ctx)
		if err == nil {
			if reconnect {
				// The gap in the stream is also a gap in what jm knows
				// about the guest: it may have rebooted, taking its pf
				// state with it while the containers came back on the same
				// addresses. Resync with the anchor written unconditionally
				// (ADR 0004: a full re-sync on reconnect).
				deb.TriggerForce()
			}
			err = readEvents(rc, deb, &backoff, cfg.MinBackoff)
			_ = rc.Close()
		}
		if ctx.Err() != nil {
			return
		}
		cfg.Log.Printf("event stream: %v; reconnecting in %s", err, backoff)
		reconnect = true
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
