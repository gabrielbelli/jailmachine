package forwarder

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// StateVersion is the forwards.json schema version.
const StateVersion = 1

// Entry is one mapping the forwarder owns, with the outcome of its last
// expose attempt.
type Entry struct {
	Proto  string `json:"proto"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
	// Error is the last expose/unexpose failure (host port in use, provider
	// unreachable), "" when the mapping is in place. Failed mappings are
	// retried on every resync.
	Error string `json:"error,omitempty"`
	// Since is when the entry was created (or last changed status).
	Since time.Time `json:"since"`
}

// Mapping returns the provider mapping for the entry.
func (e Entry) Mapping() netprov.Mapping {
	return netprov.Mapping{Proto: e.Proto, Local: e.Local, Remote: e.Remote}
}

// Status is the one-word status "jm ports" prints.
func (e Entry) Status() string {
	if e.Error != "" {
		return "error: " + e.Error
	}
	return "ok"
}

// State is the persisted owned set.
type State struct {
	Version int       `json:"version"`
	Updated time.Time `json:"updated"`
	// PublishAddr is the host address the forwarder that wrote this file
	// was started with. The record's value can be changed at any time, but
	// a running forwarder goes on binding the one it booted with, so
	// "jm ports" and "jm inspect" read this to say what is really bound
	// and mark the record's value as pending.
	PublishAddr string  `json:"publish_addr,omitempty"`
	Owned       []Entry `json:"mappings"`
}

// Load reads forwards.json; a missing file is an empty state, not an
// error.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: StateVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.Version == 0 {
		st.Version = StateVersion
	}
	return &st, nil
}

// Save writes the state atomically (temp file + rename), like the machine
// record.
func (st *State) Save(path string) error {
	st.Version = StateVersion
	st.Updated = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, StateFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Errors returns the entries whose last attempt failed.
func (st *State) Errors() []Entry {
	var out []Entry
	for _, e := range st.Owned {
		if e.Error != "" {
			out = append(out, e)
		}
	}
	return out
}

// index returns the owned entries keyed by mapping.
func (st *State) index() map[string]*Entry {
	idx := make(map[string]*Entry, len(st.Owned))
	for i := range st.Owned {
		idx[key(st.Owned[i].Mapping())] = &st.Owned[i]
	}
	return idx
}
