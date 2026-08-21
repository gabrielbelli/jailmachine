package forwarder

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// Result summarises one convergence for the log.
type Result struct {
	Exposed   []netprov.Mapping
	Unexposed []netprov.Mapping
	Failed    []netprov.Mapping
	// External are desired mappings that already exist in the provider but
	// were not created by us; they are left alone and not adopted.
	External []netprov.Mapping
	// Skipped are published ports that cannot be reached from the host at
	// all (see Plan), seen for the first time. They are recorded in the
	// state so that "jm ports" can explain them, and reported here so the
	// forwarder says so once when it happens rather than leaving the user
	// to discover a dead port with curl.
	Skipped []Entry
}

func (r Result) changed() bool {
	return len(r.Exposed)+len(r.Unexposed)+len(r.Failed)+len(r.Skipped) > 0
}

func (r Result) String() string {
	var parts []string
	if len(r.Exposed) > 0 {
		parts = append(parts, fmt.Sprintf("exposed %v", r.Exposed))
	}
	if len(r.Unexposed) > 0 {
		parts = append(parts, fmt.Sprintf("unexposed %v", r.Unexposed))
	}
	if len(r.Failed) > 0 {
		parts = append(parts, fmt.Sprintf("failed %v", r.Failed))
	}
	for _, e := range r.Skipped {
		parts = append(parts, fmt.Sprintf("warning: %s %s is published in the guest but not on the host: %s",
			e.Proto, e.Local, e.Error))
	}
	if len(r.External) > 0 {
		parts = append(parts, fmt.Sprintf("left alone (not ours) %v", r.External))
	}
	if len(parts) == 0 {
		return "no change"
	}
	return strings.Join(parts, "; ")
}

// Converge makes the provider's mapping table match desired, touching only
// mappings recorded in st: desired mappings missing from the provider are
// exposed (and recorded as owned before the call, so a crash in between
// cannot leak an unowned mapping); owned mappings no longer desired are
// unexposed and forgotten; mappings in the provider that we never created
// (the SSH port, manual additions) are never removed. Expose failures are
// recorded per entry and retried on the next call. st is saved to statePath
// whenever it changes.
func Converge(ctx context.Context, p netprov.Provider, m *machine.Machine, desired []netprov.Mapping, st *State, statePath string) (Result, error) {
	return ConvergeWith(ctx, p, m, Plan{Mappings: desired}, st, statePath)
}

// ConvergeWith is Converge over a whole Plan: the unpublishable entries are
// kept in st (with their Error, so "jm ports" lists them) but never
// exposed, and dropped once podman no longer reports them, while a mapping
// whose guest-side redirect is not in place yet keeps its host leg and
// carries the reason as its error until the next reconcile fixes it.
func ConvergeWith(ctx context.Context, p netprov.Provider, m *machine.Machine, pl Plan, st *State, statePath string) (Result, error) {
	desired, skipped := pl.Mappings, pl.Unpublishable
	var res Result
	live, err := p.List(ctx, m)
	if err != nil {
		return res, fmt.Errorf("listing provider mappings: %w", err)
	}
	liveSet := make(map[string]bool, len(live))
	for _, mp := range live {
		liveSet[key(mp)] = true
	}
	want := make(map[string]netprov.Mapping, len(desired))
	for _, mp := range desired {
		want[key(mp)] = mp
	}
	skip := make(map[string]Entry, len(skipped))
	for _, e := range skipped {
		skip[key(e.Mapping())] = e
	}
	now := time.Now().UTC()
	dirty := false

	// Stale: owned but no longer desired.
	var keep []Entry
	for _, e := range st.Owned {
		k := key(e.Mapping())
		if _, ok := want[k]; ok {
			keep = append(keep, e)
			continue
		}
		if s, ok := skip[k]; ok {
			if e.Error != s.Error {
				e.Error = s.Error
				e.Since = now
				dirty = true
			}
			keep = append(keep, e)
			delete(skip, k)
			continue
		}
		dirty = true
		if e.Remote == "" { // unpublishable entry, never exposed
			continue
		}
		if liveSet[k] {
			if err := p.Unexpose(ctx, m, e.Mapping()); err != nil {
				e.Error = "unexpose: " + err.Error()
				e.Since = now
				keep = append(keep, e)
				res.Failed = append(res.Failed, e.Mapping())
				continue
			}
		}
		res.Unexposed = append(res.Unexposed, e.Mapping())
	}
	for _, k := range sortedKeys(skip) {
		s := skip[k]
		s.Since = now
		keep = append(keep, s)
		res.Skipped = append(res.Skipped, s)
		dirty = true
	}
	st.Owned = keep

	// Missing: desired but not live. Record ownership first. New entries are
	// collected and appended only after the loop: appending to st.Owned in
	// the loop can reallocate its backing array and orphan the *Entry
	// pointers idx hands out, silently dropping the status updates below.
	idx := st.index()
	var todo []netprov.Mapping
	var added []Entry
	for _, k := range sortedKeys(want) {
		mp := want[k]
		e, owned := idx[k]
		if liveSet[k] {
			if !owned {
				res.External = append(res.External, mp)
			} else if want := pl.Pending[k]; e.Error != want {
				e.Error = want
				e.Since = now
				dirty = true
			}
			continue
		}
		if !owned {
			added = append(added, Entry{Proto: mp.Proto, Local: mp.Local, Remote: mp.Remote, Since: now})
			dirty = true
		}
		todo = append(todo, mp)
	}
	st.Owned = append(st.Owned, added...)
	if dirty {
		if err := st.Save(statePath); err != nil {
			return res, fmt.Errorf("saving %s: %w", statePath, err)
		}
		dirty = false
	}
	idx = st.index()
	for _, mp := range todo {
		e := idx[key(mp)]
		if err := p.Expose(ctx, m, mp); err != nil {
			msg := err.Error()
			if e.Error != msg {
				e.Error = msg
				e.Since = now
				dirty = true
			}
			res.Failed = append(res.Failed, mp)
			continue
		}
		if want := pl.Pending[key(mp)]; e.Error != want {
			e.Error = want
			e.Since = now
			dirty = true
		}
		res.Exposed = append(res.Exposed, mp)
	}
	if dirty {
		if err := st.Save(statePath); err != nil {
			return res, fmt.Errorf("saving %s: %w", statePath, err)
		}
	}
	return res, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Release unexposes every owned mapping the provider still lists (best
// effort, continuing on error) and clears the owned set. Owned entries the
// provider does not list (unpublishable ones, conflicts that never made it
// in) are just forgotten. "jm stop" and "jm rm" call it after terminating
// the forwarder, while the provider is still up.
func Release(ctx context.Context, p netprov.Provider, m *machine.Machine, statePath string) error {
	st, err := Load(statePath)
	if err != nil {
		return err
	}
	if len(st.Owned) == 0 {
		return nil
	}
	live, err := p.List(ctx, m)
	if err != nil {
		return fmt.Errorf("forwarder: listing provider mappings: %w", err)
	}
	liveSet := make(map[string]bool, len(live))
	for _, mp := range live {
		liveSet[key(mp)] = true
	}
	var errs []string
	for _, e := range st.Owned {
		if e.Remote == "" || !liveSet[key(e.Mapping())] {
			continue // never exposed, or already gone
		}
		if err := p.Unexpose(ctx, m, e.Mapping()); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Mapping(), err))
		}
	}
	st.Owned = nil
	if err := st.Save(statePath); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf("forwarder: releasing mappings: %s", strings.Join(errs, "; "))
	}
	return nil
}
