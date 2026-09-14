package sleeper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Status is sleeper.json: what the sleeper last reported. "jm inspect" and
// "jm doctor" read it and never ask the sleeper. Fields a later step fills in
// (the idle ones) are omitted while empty.
type Status struct {
	// PID is the sleeper that wrote the file.
	PID int `json:"pid"`
	// Mode is the sleeper's current mode.
	Mode Mode `json:"mode"`
	// UpdatedAt is when the file was written.
	UpdatedAt time.Time `json:"updated_at"`
	// State and SuspendedAt describe the last suspend the sleeper ran, while
	// the machine stays suspended.
	State       string     `json:"state,omitempty"`
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	// Held is the number of connections waiting for a wake.
	Held int `json:"held,omitempty"`

	IdleSeconds             int64      `json:"idle_seconds,omitempty"`
	IdleSuspendAfterSeconds int64      `json:"idle_suspend_after_seconds,omitempty"`
	Blockers                []string   `json:"blockers,omitempty"`
	LastActivity            *time.Time `json:"last_activity,omitempty"`
	IdleUnavailable         string     `json:"idle_unavailable,omitempty"`
	DisabledReason          string     `json:"disabled_reason,omitempty"`
	Penalty                 int        `json:"penalty,omitempty"`

	LastSuspendError string `json:"last_suspend_error,omitempty"`
	LastSuspendMS    int64  `json:"last_suspend_ms,omitempty"`
	// LastWakeBy is what woke the machine last: socket, ssh-port, wrapper
	// or jm start.
	LastWakeBy   string `json:"last_wake_by,omitempty"`
	LastResumeMS int64  `json:"last_resume_ms,omitempty"`
	// SSHWake is whether a connection to the SSH port wakes the machine: it
	// is false when the port could not be taken while suspended.
	SSHWake bool `json:"ssh_wake"`
}

// WriteStatus writes st to path atomically (temporary file, rename).
func WriteStatus(path string, st Status) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sleeper-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// LoadStatus reads sleeper.json.
func LoadStatus(path string) (Status, error) {
	var st Status
	data, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}
