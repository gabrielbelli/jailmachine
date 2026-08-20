package netprov

import (
	"context"
	"errors"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

type fake struct{ name string }

func (f fake) Name() string                 { return f.name }
func (fake) Preflight() error               { return nil }
func (fake) Logs(*machine.Machine) []string { return nil }
func (fake) Capabilities() Capabilities     { return Capabilities{} }
func (fake) Start(context.Context, *machine.Machine) (backend.NetAttachment, Endpoint, error) {
	return backend.NetAttachment{}, Endpoint{}, nil
}
func (fake) Stop(context.Context, *machine.Machine) error              { return nil }
func (fake) State(*machine.Machine) (backend.State, error)             { return backend.Stopped, nil }
func (fake) Endpoint(*machine.Machine) (Endpoint, error)               { return Endpoint{}, nil }
func (fake) Expose(context.Context, *machine.Machine, Mapping) error   { return ErrUnsupported }
func (fake) Unexpose(context.Context, *machine.Machine, Mapping) error { return ErrUnsupported }
func (fake) List(context.Context, *machine.Machine) ([]Mapping, error) { return nil, ErrUnsupported }

func TestRegistry(t *testing.T) {
	Register(fake{"fake"})
	p, err := Get("fake")
	if err != nil || p.Name() != "fake" {
		t.Fatalf("Get = %v, %v", p, err)
	}
	if _, err := Get("nope"); err == nil {
		t.Fatal("expected unknown provider error")
	}
	if n := Names(); len(n) != 1 || n[0] != "fake" {
		t.Fatalf("Names = %v", n)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration must panic")
		}
	}()
	Register(fake{"fake"})
}

func TestDefaultForHost(t *testing.T) {
	t.Setenv(ProviderEnv, "")
	if got := DefaultForHost(); got != "gvproxy" {
		t.Fatalf("default = %s", got)
	}
	t.Setenv(ProviderEnv, "user")
	if got := DefaultForHost(); got != "user" {
		t.Fatalf("override = %s", got)
	}
}

func TestMappingString(t *testing.T) {
	m := Mapping{Proto: "tcp", Local: "127.0.0.1:8080", Remote: "192.168.127.2:80"}
	if m.String() != "tcp 127.0.0.1:8080 -> 192.168.127.2:80" {
		t.Fatalf("String = %q", m.String())
	}
	if err := (fake{}).Expose(context.Background(), nil, m); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}
