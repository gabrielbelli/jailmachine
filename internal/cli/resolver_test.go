package cli

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/resolver"
)

// The doctor probes talk to a real resolver over the loopback: no name
// leaves this machine, but the wire path "jm doctor" uses is exercised.
func TestDoctorProbesARunningResolver(t *testing.T) {
	alias := netip.MustParseAddr("192.168.127.254")
	h := resolver.NewHandler(resolver.Config{
		HostAlias: alias,
		Aliases:   resolver.DefaultAliases(alias, netip.Addr{}, nil),
	})
	srv, err := resolver.Listen(h, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = srv.Close()
	})

	qctx, qcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer qcancel()

	ips, err := queryResolver(qctx, srv.Addr(), resolver.ProbeName)
	if err != nil || len(ips) != 1 || ips[0] != alias.String() {
		t.Fatalf("queryResolver(%s) = %v, %v", resolver.ProbeName, ips, err)
	}
	// The resolution path is asked of the process that answers, not read
	// from this one's build tags (ADR 0008).
	mode, err := resolverMode(qctx, srv.Addr())
	if err != nil {
		t.Fatalf("resolverMode: %v", err)
	}
	if mode != resolver.SystemMode() {
		t.Errorf("resolverMode = %q, want %q", mode, resolver.SystemMode())
	}
}

func TestSameAddrs(t *testing.T) {
	want := []netip.Addr{netip.MustParseAddr("10.0.0.5"), netip.MustParseAddr("192.0.2.7")}
	if !sameAddrs([]string{"192.0.2.7", "10.0.0.5"}, want) {
		t.Error("the same addresses in another order were reported as different")
	}
	if sameAddrs([]string{"10.0.0.5"}, want) {
		t.Error("a missing address was reported as parity")
	}
	if sameAddrs([]string{"10.0.0.5", "10.0.0.6"}, want) {
		t.Error("a different address was reported as parity")
	}
}
