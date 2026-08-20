package gvproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// Control API routes (gvisor-tap-vsock pkg/services/forwarder).
const (
	exposePath   = "/services/forwarder/expose"
	unexposePath = "/services/forwarder/unexpose"
	listPath     = "/services/forwarder/all"
)

// Client talks to gvproxy's HTTP control API over its unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the API socket at sock.
func NewClient(sock string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// wire is the JSON shape the forwarder API speaks.
type wire struct {
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	Protocol string `json:"protocol,omitempty"`
}

func toWire(m netprov.Mapping) wire {
	proto := m.Proto
	if proto == "" {
		proto = "tcp"
	}
	return wire{Local: m.Local, Remote: m.Remote, Protocol: proto}
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	// The host is a placeholder: the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://gvproxy"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gvproxy api: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("gvproxy api: %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(out))
	}
	return out, nil
}

// Expose adds a host->guest mapping.
func (c *Client) Expose(ctx context.Context, m netprov.Mapping) error {
	_, err := c.do(ctx, http.MethodPost, exposePath, toWire(m))
	return err
}

// Unexpose removes a mapping.
func (c *Client) Unexpose(ctx context.Context, m netprov.Mapping) error {
	_, err := c.do(ctx, http.MethodPost, unexposePath, toWire(m))
	return err
}

// List returns the current mapping table.
func (c *Client) List(ctx context.Context) ([]netprov.Mapping, error) {
	out, err := c.do(ctx, http.MethodGet, listPath, nil)
	if err != nil {
		return nil, err
	}
	var ws []wire
	if err := json.Unmarshal(out, &ws); err != nil {
		return nil, fmt.Errorf("gvproxy api: decoding %s: %w", listPath, err)
	}
	ms := make([]netprov.Mapping, 0, len(ws))
	for _, w := range ws {
		proto := w.Protocol
		if proto == "" {
			proto = "tcp"
		}
		ms = append(ms, netprov.Mapping{Proto: proto, Local: w.Local, Remote: w.Remote})
	}
	return ms, nil
}
