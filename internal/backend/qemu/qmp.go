package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// qmpTimeout bounds the whole QMP exchange; a wedged guest must not block
// Stop's escalation path.
const qmpTimeout = 5 * time.Second

type qmpCommand struct {
	Execute   string `json:"execute"`
	Arguments any    `json:"arguments,omitempty"`
}

type qmpResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error,omitempty"`
	// QMP is a greeting/event-emitting protocol; fields below mark frames
	// that are not command replies.
	QMP   json.RawMessage `json:"QMP,omitempty"`
	Event string          `json:"event,omitempty"`
}

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

// ExecuteCommands runs the given QMP commands in order over one connection.
func ExecuteCommands(ctx context.Context, sock string, commands ...qmpCommand) error {
	d := net.Dialer{Timeout: qmpTimeout}
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return fmt.Errorf("qmp: connect %s: %w", sock, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(qmpTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	// The server speaks first with a {"QMP": {...}} greeting.
	var greeting qmpResponse
	if err := dec.Decode(&greeting); err != nil {
		return fmt.Errorf("qmp: reading greeting: %w", err)
	}
	if greeting.QMP == nil {
		return fmt.Errorf("qmp: unexpected first frame (no greeting)")
	}

	for _, c := range commands {
		if err := enc.Encode(c); err != nil {
			return fmt.Errorf("qmp: sending %s: %w", c.Execute, err)
		}
		for {
			var resp qmpResponse
			if err := dec.Decode(&resp); err != nil {
				return fmt.Errorf("qmp: reading reply to %s: %w", c.Execute, err)
			}
			if resp.Event != "" {
				continue // asynchronous event interleaved with replies
			}
			if resp.Error != nil {
				return fmt.Errorf("qmp: %s: %s: %s", c.Execute, resp.Error.Class, resp.Error.Desc)
			}
			break
		}
	}
	return nil
}
