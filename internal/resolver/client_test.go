package resolver

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestAskAddrsAndMode(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{"a.example": {net.ParseIP("192.0.2.5")}}}
	srv, _ := serveTest(t, sys)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := AskAddrs(ctx, srv.Addr(), "a.example")
	if err != nil || len(got) != 1 || got[0].String() != "192.0.2.5" {
		t.Fatalf("AskAddrs = %v, %v", got, err)
	}
	// The question goes on the wire as asked: no search list, and nothing
	// answered out of this process's /etc/hosts (ADR 0008).
	if got, err = AskAddrs(ctx, srv.Addr(), "localhost"); err == nil {
		t.Errorf("localhost was answered locally as %v instead of by the server", got)
	} else if derr, ok := err.(*net.DNSError); !ok || !derr.IsNotFound {
		t.Errorf("a name the server does not know = %v, want a not-found", err)
	}
	mode, err := AskMode(ctx, srv.Addr())
	if err != nil || mode != SystemMode() {
		t.Errorf("AskMode = %q, %v; want %q", mode, err, SystemMode())
	}
}

// A reply too large for a datagram is retried over TCP, as any client must.
func TestAskFallsBackToTCP(t *testing.T) {
	var many []net.IP
	for i := 0; i < 400; i++ {
		many = append(many, net.IPv4(10, 2, byte(i/256), byte(i%256)))
	}
	sys := &fakeSystem{}
	sys.setIPs("big.example", many)
	srv, _ := serveTest(t, sys)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := AskAddrs(ctx, srv.Addr(), "big.example")
	if err != nil {
		t.Fatalf("AskAddrs: %v", err)
	}
	if len(got) != len(many) {
		t.Errorf("AskAddrs returned %d addresses, want %d", len(got), len(many))
	}
}

func TestParseReplyRejectsAForeignReply(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	msg := ask(t, h, "host.docker.internal.", dnsmessage.TypeA) // built with ID 0x1234
	if _, _, err := parseReply(msg, 0x4321, "host.docker.internal"); err == nil {
		t.Error("a reply to another query was accepted")
	}
	if _, _, err := parseReply(msg, 0x1234, "host.docker.internal"); err != nil {
		t.Errorf("the matching reply was rejected: %v", err)
	}
}
