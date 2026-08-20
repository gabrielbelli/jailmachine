package user

import (
	"context"
	"errors"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

func TestUserProvider(t *testing.T) {
	p, err := netprov.Get(Name)
	if err != nil {
		t.Fatal(err)
	}
	m := machine.Defaults()
	m.SSHPort = 2299
	ctx := context.Background()
	att, ep, err := p.Start(ctx, &m)
	if err != nil {
		t.Fatal(err)
	}
	want := backend.NetAttachment{Kind: backend.KindUser, HostFwdAddr: "127.0.0.1", HostFwdSSH: 2299, MAC: m.MAC}
	if att != want {
		t.Fatalf("attachment = %+v, want %+v", att, want)
	}
	if ep.SSHHost != "127.0.0.1" || ep.SSHPort != 2299 || ep.APISocket != "" || ep.GuestIP != "" {
		t.Fatalf("endpoint = %+v", ep)
	}
	if st, _ := p.State(&m); st != backend.Running {
		t.Fatalf("state = %s", st)
	}
	if err := p.Expose(ctx, &m, netprov.Mapping{}); !errors.Is(err, netprov.ErrUnsupported) {
		t.Fatalf("Expose err = %v", err)
	}
	if _, err := p.List(ctx, &m); !errors.Is(err, netprov.ErrUnsupported) {
		t.Fatalf("List err = %v", err)
	}
	if err := p.Stop(ctx, &m); err != nil {
		t.Fatal(err)
	}
}
