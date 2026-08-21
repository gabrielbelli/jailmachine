package resolver

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// serveTest brings the real UDP and TCP listeners up on the host loopback
// with a fake host resolver behind them: no name is ever resolved off this
// machine, but the wire path a guest uses is exercised end to end.
func serveTest(t *testing.T, sys System) (*Server, *net.Resolver) {
	t.Helper()
	h := NewHandler(Config{
		HostAlias: hostAlias,
		Gateway:   gatewayIP,
		Aliases:   DefaultAliases(hostAlias, gatewayIP, nil),
		System:    sys,
	})
	srv, err := Listen(h, "127.0.0.1", 0)
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
	addr := srv.Addr()
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
		},
	}
	return srv, r
}

func TestServeAnswersOverUDPAndTCP(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{"a.example": {net.ParseIP("192.0.2.5")}}}
	srv, r := serveTest(t, sys)
	if _, err := netip.ParseAddrPort(srv.Addr()); err != nil {
		t.Fatalf("Addr() = %q: %v", srv.Addr(), err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ips, err := r.LookupIP(ctx, "ip4", "a.example.")
	if err != nil || len(ips) != 1 || ips[0].String() != "192.0.2.5" {
		t.Fatalf("udp lookup = %v, %v", ips, err)
	}
	// The alias is answered without the host resolver being asked at all.
	ips, err = r.LookupIP(ctx, "ip4", "host.docker.internal.")
	if err != nil || len(ips) != 1 || ips[0].String() != hostAlias.String() {
		t.Fatalf("alias lookup = %v, %v", ips, err)
	}
	// A large answer forces the resolver on to TCP; the listeners share a
	// port, so the retry lands on the same server.
	var many []net.IP
	for i := 0; i < 120; i++ {
		many = append(many, net.IPv4(10, 1, byte(i/256), byte(i%256)))
	}
	sys.ips["big.example"] = many
	ips, err = r.LookupIP(ctx, "ip4", "big.example.")
	if err != nil {
		t.Fatalf("tcp lookup: %v", err)
	}
	if len(ips) != len(many) {
		t.Errorf("tcp lookup returned %d addresses, want %d", len(ips), len(many))
	}
}

func TestServeReportsNotFound(t *testing.T) {
	_, r := serveTest(t, &fakeSystem{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := r.LookupIP(ctx, "ip4", "nope.example.")
	var derr *net.DNSError
	if err == nil {
		t.Fatal("a missing name resolved")
	}
	if !strings.Contains(err.Error(), "no such host") {
		t.Errorf("error = %v, want a not-found", err)
	}
	if ok := asDNSError(err, &derr); ok && !derr.IsNotFound {
		t.Errorf("error is not marked as not-found: %+v", derr)
	}
}

func asDNSError(err error, out **net.DNSError) bool {
	d, ok := err.(*net.DNSError)
	if ok {
		*out = d
	}
	return ok
}

// Garbage on the wire must not take the listener down.
func TestServeSurvivesGarbage(t *testing.T) {
	srv, r := serveTest(t, &fakeSystem{ips: map[string][]net.IP{"a.example": {net.ParseIP("192.0.2.5")}}})
	conn, err := net.Dial("udp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{0x00, 0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	tcp, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tcp.Write([]byte{0x00, 0x03, 0x00, 0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	_ = tcp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ips, err := r.LookupIP(ctx, "ip4", "a.example."); err != nil || len(ips) != 1 {
		t.Fatalf("the server stopped answering after garbage: %v, %v", ips, err)
	}
}

// Listen prefers the port of the previous run so that a guest configured
// before a restart keeps working.
func TestListenPrefersAPort(t *testing.T) {
	h := NewHandler(Config{HostAlias: hostAlias, System: &fakeSystem{}})
	first, err := Listen(h, "127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	port := first.Port()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Listen(h, "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Port() != port {
		t.Errorf("port = %d, want the previous %d", again.Port(), port)
	}
}
