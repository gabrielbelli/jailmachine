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
	Execute string `json:"execute"`
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

// Execute runs the given QMP commands in order over one connection.
func Execute(ctx context.Context, sock string, commands ...string) error {
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
		if err := enc.Encode(qmpCommand{Execute: c}); err != nil {
			return fmt.Errorf("qmp: sending %s: %w", c, err)
		}
		for {
			var resp qmpResponse
			if err := dec.Decode(&resp); err != nil {
				return fmt.Errorf("qmp: reading reply to %s: %w", c, err)
			}
			if resp.Event != "" {
				continue // asynchronous event interleaved with replies
			}
			if resp.Error != nil {
				return fmt.Errorf("qmp: %s: %s: %s", c, resp.Error.Class, resp.Error.Desc)
			}
			break
		}
	}
	return nil
}
