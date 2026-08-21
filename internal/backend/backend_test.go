package backend

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

type fake struct{}

func (fake) Name() string                                                 { return "fake" }
func (fake) Preflight() error                                             { return nil }
func (fake) Start(context.Context, *machine.Machine, NetAttachment) error { return nil }
func (fake) Stop(context.Context, *machine.Machine, bool) error           { return nil }
func (fake) State(*machine.Machine) (State, error)                        { return Stopped, nil }
func (fake) ConsolePath(*machine.Machine) string                          { return "" }
func (fake) Logs(*machine.Machine) []string                               { return nil }
func (fake) Capabilities() Capabilities                                   { return Capabilities{} }

func TestRegistry(t *testing.T) {
	Register(fake{})
	if _, err := Get("fake"); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("missing"); err == nil {
		t.Fatal("expected error for unknown backend")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register should panic")
		}
	}()
	Register(fake{})
}

func TestDefaultForHost(t *testing.T) {
	t.Setenv(BackendEnv, "")
	got, err := DefaultForHost()
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if err != nil || got != "qemu" {
			t.Fatalf("DefaultForHost = %q, %v; want qemu", got, err)
		}
	} else if !errors.Is(err, ErrNoBackend) || !strings.Contains(err.Error(), BackendEnv) {
		t.Fatalf("DefaultForHost = %q, %v; want ErrNoBackend naming %s", got, err, BackendEnv)
	}
	t.Setenv(BackendEnv, "fake")
	if got, err := DefaultForHost(); err != nil || got != "fake" {
		t.Fatalf("override ignored: %q, %v", got, err)
	}
}
