package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// qmpTimeout bounds each QMP exchange; a wedged guest must not block Stop's
// escalation path.
const qmpTimeout = 5 * time.Second

type qmpCommand struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

type qmpResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *QMPError       `json:"error,omitempty"`
	// QMP is a greeting/event-emitting protocol; fields below mark frames
	// that are not command replies.
	QMP   json.RawMessage `json:"QMP,omitempty"`
	Event string          `json:"event,omitempty"`
}

// QMPError is an error reply from QEMU.
type QMPError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

func (e *QMPError) Error() string { return e.Class + ": " + e.Desc }

// Monitor is one negotiated QMP session. It is not safe for concurrent use.
type Monitor struct {
	conn net.Conn
	dec  *json.Decoder
	enc  *json.Encoder
}

// deadlineFor is min(ctx deadline, now + qmpTimeout).
func deadlineFor(ctx context.Context) time.Time {
	deadline := time.Now().Add(qmpTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	return deadline
}

// DialMonitor connects to a QMP unix socket, reads the greeting and leaves
// capabilities negotiation mode (qmp_capabilities).
func DialMonitor(ctx context.Context, sock string) (*Monitor, error) {
	d := net.Dialer{Timeout: qmpTimeout}
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("qmp: connect %s: %w", sock, err)
	}
	q := &Monitor{conn: conn, dec: json.NewDecoder(conn), enc: json.NewEncoder(conn)}
	_ = conn.SetDeadline(deadlineFor(ctx))
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	// The server speaks first with a {"QMP": {...}} greeting.
	var greeting qmpResponse
	err = q.dec.Decode(&greeting)
	stop()
	switch {
	case err != nil:
		conn.Close()
		return nil, fmt.Errorf("qmp: reading greeting: %w", err)
	case greeting.QMP == nil:
		conn.Close()
		return nil, errors.New("qmp: unexpected first frame (no greeting)")
	}
	if err := q.Call(ctx, "qmp_capabilities", nil, nil); err != nil {
		conn.Close()
		return nil, err
	}
	return q, nil
}

// Close ends the session.
func (q *Monitor) Close() error { return q.conn.Close() }

// Call sends one command and waits for its reply, skipping asynchronous
// events. The deadline is the earlier of ctx's and qmpTimeout from now. An
// error reply is returned as a *QMPError (wrapped). When ret is not nil the
// reply's "return" value is decoded into it.
func (q *Monitor) Call(ctx context.Context, execute string, args, ret any) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("qmp: %s: %w", execute, err)
	}
	_ = q.conn.SetDeadline(deadlineFor(ctx))
	stop := context.AfterFunc(ctx, func() { _ = q.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := q.enc.Encode(qmpCommand{Execute: execute, Arguments: args}); err != nil {
		return fmt.Errorf("qmp: sending %s: %w", execute, err)
	}
	for {
		var resp qmpResponse
		if err := q.dec.Decode(&resp); err != nil {
			return fmt.Errorf("qmp: reading reply to %s: %w", execute, err)
		}
		if resp.Event != "" || resp.QMP != nil {
			continue // asynchronous event interleaved with replies
		}
		if resp.Error != nil {
			return fmt.Errorf("qmp: %s: %w", execute, resp.Error)
		}
		if ret != nil && len(resp.Return) > 0 {
			if err := json.Unmarshal(resp.Return, ret); err != nil {
				return fmt.Errorf("qmp: decoding reply to %s: %w", execute, err)
			}
		}
		return nil
	}
}

// QueryStatus returns the run state: "prelaunch", "running", "paused",
// "inmigrate", "postmigrate" and so on.
func (q *Monitor) QueryStatus(ctx context.Context) (string, error) {
	var r struct {
		Status string `json:"status"`
	}
	if err := q.Call(ctx, "query-status", nil, &r); err != nil {
		return "", err
	}
	return r.Status, nil
}

// QueryVersion returns QEMU's version as "major.minor.micro".
func (q *Monitor) QueryVersion(ctx context.Context) (string, error) {
	var r struct {
		QEMU struct {
			Major, Minor, Micro int
		} `json:"qemu"`
	}
	if err := q.Call(ctx, "query-version", nil, &r); err != nil {
		return "", err
	}
	return strconv.Itoa(r.QEMU.Major) + "." + strconv.Itoa(r.QEMU.Minor) + "." + strconv.Itoa(r.QEMU.Micro), nil
}

// AliasTarget returns the versioned machine type that alias ("virt") stands
// for in this QEMU, from query-machines.
func (q *Monitor) AliasTarget(ctx context.Context, alias string) (string, error) {
	var machines []struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	}
	if err := q.Call(ctx, "query-machines", nil, &machines); err != nil {
		return "", err
	}
	for _, m := range machines {
		if m.Alias == alias && m.Name != "" {
			return m.Name, nil
		}
	}
	return "", fmt.Errorf("qmp: no machine type is aliased %q", alias)
}

// Migration capabilities used for a save to a file.
const (
	capMappedRAM = "mapped-ram"
	capMultifd   = "multifd"
)

// FileMigrationCapable returns nil when this QEMU lists both capabilities a
// migration to a file needs (mapped-ram and multifd).
func (q *Monitor) FileMigrationCapable(ctx context.Context) error {
	var caps []struct {
		Capability string `json:"capability"`
	}
	if err := q.Call(ctx, "query-migrate-capabilities", nil, &caps); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range caps {
		have[c.Capability] = true
	}
	for _, want := range []string{capMappedRAM, capMultifd} {
		if !have[want] {
			return fmt.Errorf("qmp: migration capability %s is not available", want)
		}
	}
	return nil
}

// SetFileMigration turns on mapped-ram and multifd and sets the number of
// multifd channels.
func (q *Monitor) SetFileMigration(ctx context.Context, channels int) error {
	caps := []map[string]any{
		{"capability": capMappedRAM, "state": true},
		{"capability": capMultifd, "state": true},
	}
	if err := q.Call(ctx, "migrate-set-capabilities", map[string]any{"capabilities": caps}, nil); err != nil {
		return err
	}
	return q.Call(ctx, "migrate-set-parameters", map[string]any{"multifd-channels": channels}, nil)
}

// MigrationInfo is the part of query-migrate jm reads.
type MigrationInfo struct {
	Status         string   `json:"status"`
	ErrorDesc      string   `json:"error-desc,omitempty"`
	BlockedReasons []string `json:"blocked-reasons,omitempty"`
	TotalTime      int64    `json:"total-time,omitempty"`
	Downtime       int64    `json:"downtime,omitempty"`
	RAM            *struct {
		Transferred int64 `json:"transferred"`
		Total       int64 `json:"total"`
	} `json:"ram,omitempty"`
}

// QueryMigrate returns the state of the current or last migration.
func (q *Monitor) QueryMigrate(ctx context.Context) (MigrationInfo, error) {
	var info MigrationInfo
	err := q.Call(ctx, "query-migrate", nil, &info)
	return info, err
}

// Migrate starts an outgoing migration to uri ("file:/path").
func (q *Monitor) Migrate(ctx context.Context, uri string) error {
	return q.Call(ctx, "migrate", map[string]any{"uri": uri}, nil)
}

// MigrateIncoming loads an incoming migration from uri into a QEMU started
// with -incoming defer.
func (q *Monitor) MigrateIncoming(ctx context.Context, uri string) error {
	return q.Call(ctx, "migrate-incoming", map[string]any{"uri": uri}, nil)
}

// MigrateCancel cancels an outgoing migration.
func (q *Monitor) MigrateCancel(ctx context.Context) error {
	return q.Call(ctx, "migrate_cancel", nil, nil)
}

// Stop pauses the guest's vCPUs.
func (q *Monitor) Stop(ctx context.Context) error { return q.Call(ctx, "stop", nil, nil) }

// Cont resumes the guest's vCPUs.
func (q *Monitor) Cont(ctx context.Context) error { return q.Call(ctx, "cont", nil, nil) }

// Quit makes QEMU exit at once, without a guest shutdown.
func (q *Monitor) Quit(ctx context.Context) error { return q.Call(ctx, "quit", nil, nil) }

// Powerdown connects to the QMP unix socket, negotiates capabilities and
// asks the guest for an ACPI power-down (the equivalent of pressing the
// power button). It returns once QEMU has acknowledged the command.
func Powerdown(ctx context.Context, sock string) error {
	return Execute(ctx, sock, "qmp_capabilities", "system_powerdown")
}

// DiskDevice is the QMP name QEMU gives the first "-drive if=virtio" (the
// root disk in Args): auto-generated "virtio<index>".
const DiskDevice = "virtio0"

// BlockResize tells a running QEMU that the backing file of device has
// grown to size bytes, so the guest sees the new capacity without a
// reboot (QEMU reads the file size only at boot).
func BlockResize(ctx context.Context, sock, device string, size int64) error {
	return ExecuteCommands(ctx, sock,
		qmpCommand{Execute: "qmp_capabilities"},
		qmpCommand{Execute: "block_resize", Arguments: map[string]any{"device": device, "size": size}},
	)
}

// Execute runs the given argument-less QMP commands in order over one
// connection.
func Execute(ctx context.Context, sock string, commands ...string) error {
	cmds := make([]qmpCommand, 0, len(commands))
	for _, c := range commands {
		cmds = append(cmds, qmpCommand{Execute: c})
	}
	return ExecuteCommands(ctx, sock, cmds...)
}

// ExecuteCommands runs the given QMP commands in order over one Monitor
// session. DialMonitor already negotiates capabilities, so a
// "qmp_capabilities" entry is accepted and skipped.
func ExecuteCommands(ctx context.Context, sock string, commands ...qmpCommand) error {
	q, err := DialMonitor(ctx, sock)
	if err != nil {
		return err
	}
	defer q.Close()
	for _, c := range commands {
		if c.Execute == "qmp_capabilities" {
			continue
		}
		if err := q.Call(ctx, c.Execute, c.Arguments, nil); err != nil {
			return err
		}
	}
	return nil
}
