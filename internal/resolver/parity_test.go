package resolver

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestNetdnsSetting(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"netdns=go", "go"},
		{"netdns=cgo", "cgo"},
		{"netdns=go+2", "go"},
		{"http2client=0,netdns=go,asyncpreemptoff=1", "go"},
		{"netdns=go,netdns=cgo", "cgo"},
		{"netdnsfoo=go", ""},
	}
	for _, c := range cases {
		if got := netdnsSetting(c.in); got != c.want {
			t.Errorf("netdnsSetting(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The mode is a run-time answer: a build that has the libSystem path still
// loses it to GODEBUG, and doctor has to see that (ADR 0008).
func TestSystemModeFollowsGODEBUG(t *testing.T) {
	t.Setenv("GODEBUG", "netdns=go")
	if got := SystemMode(); got != ModeGo {
		t.Errorf("SystemMode with GODEBUG=netdns=go = %q, want %q", got, ModeGo)
	}
	t.Setenv("GODEBUG", "")
	want := ModeGo
	if HostResolver {
		want = ModeHost
	}
	if got := SystemMode(); got != want {
		t.Errorf("SystemMode = %q, want %q", got, want)
	}
}

// The resolver reports its own resolution path, so the process serving the
// guest can be asked over the wire rather than trusted.
func TestStatusNameReportsTheResolutionPath(t *testing.T) {
	sys := &fakeSystem{}
	h := NewHandler(Config{HostAlias: hostAlias, System: sys, Mode: ModeGo})
	_, answers := reply(t, ask(t, h, StatusName+".", dnsmessage.TypeTXT))
	if len(answers) != 1 {
		t.Fatalf("%s TXT = %+v, want one record", StatusName, answers)
	}
	mode, ok := ParseStatus(answers[0].Body.(*dnsmessage.TXTResource).TXT)
	if !ok || mode != ModeGo {
		t.Errorf("status = %q, %v; want %q", mode, ok, ModeGo)
	}
	// It is a report, not an address, and it is never forwarded.
	hdr, answers := reply(t, ask(t, h, StatusName+".", dnsmessage.TypeA))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("%s A = %v with %d answers, want success and none", StatusName, hdr.RCode, len(answers))
	}
	if calls := sys.asked(); len(calls) != 0 {
		t.Errorf("the status name was forwarded to the host resolver: %v", calls)
	}
}

// Over the wire, the way "jm doctor" asks.
func TestServeAnswersTheStatusName(t *testing.T) {
	_, r := serveTest(t, &fakeSystem{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	txt, err := r.LookupTXT(ctx, StatusName+".")
	if err != nil {
		t.Fatalf("LookupTXT: %v", err)
	}
	mode, ok := ParseStatus(txt)
	if !ok || mode != SystemMode() {
		t.Errorf("status over the wire = %q, %v; want %q", mode, ok, SystemMode())
	}
}

func TestParseStatus(t *testing.T) {
	if _, ok := ParseStatus([]string{"v=spf1 -all"}); ok {
		t.Error("an unrelated TXT record was read as a status")
	}
	if mode, ok := ParseStatus([]string{"other", " mode=host "}); !ok || mode != ModeHost {
		t.Errorf("ParseStatus = %q, %v", mode, ok)
	}
}

func TestHostsFileNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	content := "# a comment\n\n127.0.0.1\tlocalhost loopback # trailing\n" +
		"255.255.255.255 broadcasthost\n10.0.0.5 build.internal localhost\nbroken\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := hostsFileNames(path)
	want := []string{"localhost", "loopback", "broadcasthost", "build.internal"}
	if len(got) != len(want) {
		t.Fatalf("hostsFileNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hostsFileNames = %v, want %v", got, want)
		}
	}
	if names := hostsFileNames(filepath.Join(t.TempDir(), "absent")); names != nil {
		t.Errorf("a missing hosts file yielded %v", names)
	}
}

// The probe has to be a name the alias table does not hold — an alias round
// trip proves the table, not that anything reached the host — and one with
// an address the guest could actually be given (ADR 0008).
func TestParityProbeSkipsAliasesAndUnreachableNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	content := "127.0.0.1 darwin.local\n0.0.0.0 ads.example\n1.2.3.4 unknown.example\n" +
		"127.0.0.1 myapp.test\n10.0.0.5 build.internal\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := HostsFile
	HostsFile = path
	t.Cleanup(func() { HostsFile = old })

	sys := &fakeSystem{ips: map[string][]net.IP{
		"darwin.local":   {net.ParseIP("127.0.0.1")},
		"ads.example":    {net.ParseIP("0.0.0.0")},
		"myapp.test":     {net.ParseIP("127.0.0.1")},
		"build.internal": {net.ParseIP("10.0.0.5")},
	}}
	aliases := DefaultAliases(hostAlias, gatewayIP, []string{"darwin", "darwin.local"})

	// darwin.local is an alias, ads.example is blocked, unknown.example is
	// not resolvable, and myapp.test is only the host alias again: the one
	// with an address of its own wins.
	name, want, ok := ParityProbe(context.Background(), sys, hostAlias, aliases)
	if !ok || name != "build.internal" || len(want) != 1 || want[0].String() != "10.0.0.5" {
		t.Fatalf("ParityProbe = %q, %v, %v", name, want, ok)
	}

	// With no such name, a loopback entry still beats nothing: it proves
	// the lookup reached the host and was rewritten for the guest.
	if err := os.WriteFile(path, []byte("127.0.0.1 darwin.local\n127.0.0.1 myapp.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, want, ok = ParityProbe(context.Background(), sys, hostAlias, aliases)
	if !ok || name != "myapp.test" || len(want) != 1 || want[0] != hostAlias {
		t.Fatalf("ParityProbe fallback = %q, %v, %v", name, want, ok)
	}

	// With nothing left to compare against, the caller is told so rather
	// than handed a name that proves nothing.
	if err := os.WriteFile(path, []byte("127.0.0.1 darwin.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if name, _, ok := ParityProbe(context.Background(), sys, hostAlias, aliases); ok {
		t.Errorf("ParityProbe found %q among aliases only", name)
	}
}

func TestGuestAddrs(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("127.0.0.1"),
		net.ParseIP("0.0.0.0"),
		net.ParseIP("169.254.1.2"),
		net.ParseIP("192.0.2.7"),
		net.ParseIP("192.0.2.7"),
		net.ParseIP("2001:db8::1"),
	}
	got := GuestAddrs(ips, hostAlias, false)
	want := []netip.Addr{hostAlias, netip.MustParseAddr("192.0.2.7")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("GuestAddrs = %v, want %v", got, want)
	}
	if got := GuestAddrs(ips, hostAlias, true); len(got) != 1 || got[0].String() != "2001:db8::1" {
		t.Errorf("GuestAddrs(v6) = %v", got)
	}
}
