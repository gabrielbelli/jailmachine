package resolver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// fakeSystem is a host resolver that answers from tables, so every test in
// this file runs without a network.
//
// Every field is behind mu: the serve tests hand the same fake to a running
// server, which reads it from one goroutine per query while the test may
// still be adding names to it (setIPs), so unguarded maps would make
// "go test -race" fail on the fixture rather than on the code under test.
type fakeSystem struct {
	mu    sync.Mutex
	ips   map[string][]net.IP
	cname map[string]string
	ptr   map[string][]string
	txt   map[string][]string
	srv   map[string][]*net.SRV
	mx    map[string][]*net.MX
	ns    map[string][]*net.NS
	err   map[string]error
	calls []string
}

func (f *fakeSystem) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

// setIPs adds a name while the server may already be serving.
func (f *fakeSystem) setIPs(host string, ips []net.IP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ips == nil {
		f.ips = map[string][]net.IP{}
	}
	f.ips[host] = ips
}

// asked returns the names the fake was asked for.
func (f *fakeSystem) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f *fakeSystem) fail(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err[name]
}

func (f *fakeSystem) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	f.record(host)
	if err := f.fail(host); err != nil {
		return nil, err
	}
	f.mu.Lock()
	ips, ok := f.ips[host]
	f.mu.Unlock()
	if !ok {
		return nil, notFound(host)
	}
	return ips, nil
}

func (f *fakeSystem) LookupCNAME(_ context.Context, host string) (string, error) {
	f.record(host)
	if err := f.fail(host); err != nil {
		return "", err
	}
	f.mu.Lock()
	c, ok := f.cname[host]
	f.mu.Unlock()
	if !ok {
		return "", notFound(host)
	}
	return c, nil
}

func (f *fakeSystem) LookupPTR(_ context.Context, addr string) ([]string, error) {
	f.record(addr)
	if err := f.fail(addr); err != nil {
		return nil, err
	}
	f.mu.Lock()
	n, ok := f.ptr[addr]
	f.mu.Unlock()
	if !ok {
		return nil, notFound(addr)
	}
	return n, nil
}

func (f *fakeSystem) LookupTXT(_ context.Context, name string) ([]string, error) {
	f.record(name)
	if err := f.fail(name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	t, ok := f.txt[name]
	f.mu.Unlock()
	if !ok {
		return nil, notFound(name)
	}
	return t, nil
}

func (f *fakeSystem) LookupSRV(_ context.Context, name string) ([]*net.SRV, error) {
	f.record(name)
	if err := f.fail(name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	s, ok := f.srv[name]
	f.mu.Unlock()
	if !ok {
		return nil, notFound(name)
	}
	return s, nil
}

func (f *fakeSystem) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	f.record(name)
	if err := f.fail(name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	m, ok := f.mx[name]
	f.mu.Unlock()
	if !ok {
		return nil, notFound(name)
	}
	return m, nil
}

func (f *fakeSystem) LookupNS(_ context.Context, name string) ([]*net.NS, error) {
	f.record(name)
	if err := f.fail(name); err != nil {
		return nil, err
	}
	f.mu.Lock()
	n, ok := f.ns[name]
	f.mu.Unlock()
	if !ok {
		return nil, notFound(name)
	}
	return n, nil
}

var (
	hostAlias = netip.MustParseAddr("192.168.127.254")
	gatewayIP = netip.MustParseAddr("192.168.127.1")
)

func newTestHandler(t *testing.T, sys System, allowV6 bool) *Handler {
	t.Helper()
	return NewHandler(Config{
		HostAlias: hostAlias,
		Gateway:   gatewayIP,
		Aliases:   DefaultAliases(hostAlias, gatewayIP, []string{"darwin", "darwin.local"}),
		AllowIPv6: allowV6,
		System:    sys,
	})
}

// query builds a wire-format question, optionally with an EDNS(0) record.
func query(t *testing.T, name string, typ dnsmessage.Type, class dnsmessage.Class, udpSize uint16) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: class}); err != nil {
		t.Fatal(err)
	}
	if udpSize > 0 {
		if err := b.StartAdditionals(); err != nil {
			t.Fatal(err)
		}
		var h dnsmessage.ResourceHeader
		if err := h.SetEDNS0(int(udpSize), dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		if err := b.OPTResource(h, dnsmessage.OPTResource{}); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// reply parses a response into its header and answer records.
func reply(t *testing.T, msg []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		t.Fatalf("parsing reply: %v", err)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatalf("skipping questions: %v", err)
	}
	answers, err := p.AllAnswers()
	if err != nil && !errors.Is(err, dnsmessage.ErrSectionDone) {
		t.Fatalf("reading answers: %v", err)
	}
	return hdr, answers
}

// addresses renders the A/AAAA records of a reply.
func addresses(t *testing.T, msg []byte) []string {
	t.Helper()
	_, answers := reply(t, msg)
	var out []string
	for _, a := range answers {
		switch body := a.Body.(type) {
		case *dnsmessage.AResource:
			out = append(out, netip.AddrFrom4(body.A).String())
		case *dnsmessage.AAAAResource:
			out = append(out, netip.AddrFrom16(body.AAAA).Unmap().String())
		}
		if a.Header.TTL != 0 {
			t.Errorf("answer TTL = %d, want 0 (parity is at the time of asking)", a.Header.TTL)
		}
	}
	return out
}

func ask(t *testing.T, h *Handler, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	return h.Answer(context.Background(), query(t, name, typ, dnsmessage.ClassINET, 0))
}

// The aliases every container expects are answered locally and never
// forwarded: forwarding them would return whatever the host thinks
// "host.docker.internal" is, which is nothing.
func TestAliasesAnsweredLocally(t *testing.T) {
	sys := &fakeSystem{}
	h := newTestHandler(t, sys, false)
	for _, name := range []string{
		"host.containers.internal.", "host.docker.internal.", "HOST.DOCKER.INTERNAL.", "darwin.local.", "darwin.",
	} {
		got := addresses(t, ask(t, h, name, dnsmessage.TypeA))
		if len(got) != 1 || got[0] != hostAlias.String() {
			t.Errorf("%s = %v, want [%s]", name, got, hostAlias)
		}
	}
	for _, name := range []string{"gateway.containers.internal.", "gateway.docker.internal."} {
		got := addresses(t, ask(t, h, name, dnsmessage.TypeA))
		if len(got) != 1 || got[0] != gatewayIP.String() {
			t.Errorf("%s = %v, want [%s]", name, got, gatewayIP)
		}
	}
	if calls := sys.asked(); len(calls) != 0 {
		t.Errorf("aliases were forwarded to the host resolver: %v", calls)
	}
}

// An alias is an IPv4 address on the provider's network: asking for its
// AAAA must say "the name exists, that record does not", not NXDOMAIN,
// which would make a dual-stack client give up on the name altogether.
func TestAliasAAAAIsEmptyNoError(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	hdr, answers := reply(t, ask(t, h, "host.docker.internal.", dnsmessage.TypeAAAA))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("AAAA of an alias = %v with %d answers, want success and none", hdr.RCode, len(answers))
	}
}

func TestForwardsToHostResolver(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{
		"internal.example.com": {net.ParseIP("10.1.2.3")},
	}}
	h := newTestHandler(t, sys, false)
	got := addresses(t, ask(t, h, "internal.example.com.", dnsmessage.TypeA))
	if len(got) != 1 || got[0] != "10.1.2.3" {
		t.Fatalf("got %v, want [10.1.2.3]", got)
	}
	if calls := sys.asked(); len(calls) != 1 || calls[0] != "internal.example.com" {
		t.Errorf("host resolver was asked %v", calls)
	}
}

// A name the host answers with a loopback address — an /etc/hosts entry, a
// service bound to 127.0.0.1 — must come back as the host alias: inside the
// guest, 127.0.0.1 is the guest.
func TestLoopbackBecomesTheHostAlias(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{
		"myapp.test": {net.ParseIP("127.0.0.1")},
		"mixed.test": {net.ParseIP("127.0.0.1"), net.ParseIP("192.168.0.9")},
	}}
	h := newTestHandler(t, sys, false)
	if got := addresses(t, ask(t, h, "myapp.test.", dnsmessage.TypeA)); len(got) != 1 || got[0] != hostAlias.String() {
		t.Errorf("myapp.test = %v, want [%s]", got, hostAlias)
	}
	got := addresses(t, ask(t, h, "mixed.test.", dnsmessage.TypeA))
	if len(got) != 2 || got[0] != hostAlias.String() || got[1] != "192.168.0.9" {
		t.Errorf("mixed.test = %v", got)
	}
}

// "0.0.0.0 <name>" is how a hosts file blocks a name (StevenBlack's list and
// every other one spell it that way) and macOS getaddrinfo hands it back
// verbatim. Parity with the host means "unreachable", so the answer is empty
// — never the host alias, which would send every blocked domain at a service
// on the Mac.
func TestBlockedHostsEntryIsNotTheHost(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{
		"ads.example.com":   {net.ParseIP("0.0.0.0")},
		"mixed.example.com": {net.ParseIP("0.0.0.0"), net.ParseIP("192.168.0.9")},
	}}
	h := newTestHandler(t, sys, false)
	hdr, answers := reply(t, ask(t, h, "ads.example.com.", dnsmessage.TypeA))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("blocked name = %v with %d answers, want success and none", hdr.RCode, len(answers))
	}
	if got := addresses(t, ask(t, h, "mixed.example.com.", dnsmessage.TypeA)); len(got) != 1 || got[0] != "192.168.0.9" {
		t.Errorf("mixed.example.com = %v, want [192.168.0.9]", got)
	}
}

// Link-local and multicast addresses are not routable from the guest.
func TestUnroutableAddressesAreDropped(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{
		"linklocal.test": {net.ParseIP("169.254.1.2")},
	}}
	h := newTestHandler(t, sys, false)
	hdr, answers := reply(t, ask(t, h, "linklocal.test.", dnsmessage.TypeA))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("link-local = %v with %d answers, want success and none", hdr.RCode, len(answers))
	}
}

func TestNXDOMAINPassesThrough(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	hdr, answers := reply(t, ask(t, h, "nope.example.", dnsmessage.TypeA))
	if hdr.RCode != dnsmessage.RCodeNameError {
		t.Errorf("rcode = %v, want NXDOMAIN", hdr.RCode)
	}
	if len(answers) != 0 {
		t.Errorf("NXDOMAIN carried %d answers", len(answers))
	}
}

// Anything that is not "no such name" is a server failure, never an
// invented answer: on a split-horizon network a guess is a wrong address.
func TestResolverFailureIsServerFailure(t *testing.T) {
	sys := &fakeSystem{err: map[string]error{
		"vpn.example": &net.DNSError{Err: "server misbehaving", Name: "vpn.example", IsTemporary: true},
	}}
	h := newTestHandler(t, sys, false)
	hdr, answers := reply(t, ask(t, h, "vpn.example.", dnsmessage.TypeA))
	if hdr.RCode != dnsmessage.RCodeServerFailure || len(answers) != 0 {
		t.Errorf("rcode = %v with %d answers, want SERVFAIL and none", hdr.RCode, len(answers))
	}
}

// The gvproxy network is IPv4 only, so AAAA answers would be addresses the
// guest cannot reach. The name still exists: NOERROR with no answer.
func TestIPv6WithheldWhenUnroutable(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{
		"dual.example":   {net.ParseIP("203.0.113.7"), net.ParseIP("2001:db8::1")},
		"v6only.example": {net.ParseIP("2001:db8::2")},
	}}
	h := newTestHandler(t, sys, false)
	hdr, answers := reply(t, ask(t, h, "dual.example.", dnsmessage.TypeAAAA))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("AAAA = %v with %d answers, want success and none", hdr.RCode, len(answers))
	}
	if got := addresses(t, ask(t, h, "dual.example.", dnsmessage.TypeA)); len(got) != 1 || got[0] != "203.0.113.7" {
		t.Errorf("A of a dual-stack name = %v", got)
	}
	// A name with only IPv6 addresses yields an empty answer, never one the
	// guest cannot route to.
	hdr, answers = reply(t, ask(t, h, "v6only.example.", dnsmessage.TypeA))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("A of a v6-only name = %v with %d answers", hdr.RCode, len(answers))
	}
	// A nonexistent name is still NXDOMAIN on AAAA.
	if hdr, _ = reply(t, ask(t, h, "nope.example.", dnsmessage.TypeAAAA)); hdr.RCode != dnsmessage.RCodeNameError {
		t.Errorf("AAAA of a missing name = %v, want NXDOMAIN", hdr.RCode)
	}
}

func TestIPv6AnsweredWhenRoutable(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{
		"dual.example": {net.ParseIP("203.0.113.7"), net.ParseIP("2001:db8::1")},
	}}
	h := newTestHandler(t, sys, true)
	got := addresses(t, ask(t, h, "dual.example.", dnsmessage.TypeAAAA))
	if len(got) != 1 || got[0] != "2001:db8::1" {
		t.Errorf("AAAA = %v, want [2001:db8::1]", got)
	}
}

func TestRecordTypesPassThrough(t *testing.T) {
	sys := &fakeSystem{
		cname: map[string]string{"www.example": "edge.example."},
		ptr:   map[string][]string{"192.168.0.9": {"printer.lan."}},
		txt:   map[string][]string{"example": {"v=spf1 -all"}},
		srv:   map[string][]*net.SRV{"_sip._tcp.example": {{Target: "sip.example.", Port: 5060, Priority: 1, Weight: 2}}},
		mx:    map[string][]*net.MX{"example": {{Host: "mail.example.", Pref: 10}}},
		ns:    map[string][]*net.NS{"example": {{Host: "ns1.example."}}},
	}
	h := newTestHandler(t, sys, false)

	_, answers := reply(t, ask(t, h, "www.example.", dnsmessage.TypeCNAME))
	if len(answers) != 1 || answers[0].Body.(*dnsmessage.CNAMEResource).CNAME.String() != "edge.example." {
		t.Errorf("CNAME = %+v", answers)
	}
	_, answers = reply(t, ask(t, h, "9.0.168.192.in-addr.arpa.", dnsmessage.TypePTR))
	if len(answers) != 1 || answers[0].Body.(*dnsmessage.PTRResource).PTR.String() != "printer.lan." {
		t.Errorf("PTR = %+v", answers)
	}
	_, answers = reply(t, ask(t, h, "example.", dnsmessage.TypeTXT))
	if len(answers) != 1 || answers[0].Body.(*dnsmessage.TXTResource).TXT[0] != "v=spf1 -all" {
		t.Errorf("TXT = %+v", answers)
	}
	_, answers = reply(t, ask(t, h, "_sip._tcp.example.", dnsmessage.TypeSRV))
	if len(answers) != 1 || answers[0].Body.(*dnsmessage.SRVResource).Port != 5060 {
		t.Errorf("SRV = %+v", answers)
	}
	_, answers = reply(t, ask(t, h, "example.", dnsmessage.TypeMX))
	if len(answers) != 1 || answers[0].Body.(*dnsmessage.MXResource).Pref != 10 {
		t.Errorf("MX = %+v", answers)
	}
	_, answers = reply(t, ask(t, h, "example.", dnsmessage.TypeNS))
	if len(answers) != 1 || answers[0].Body.(*dnsmessage.NSResource).NS.String() != "ns1.example." {
		t.Errorf("NS = %+v", answers)
	}
}

// res_search echoes the query when a name has no CNAME; that is not an
// answer and must not be reported as one.
func TestCNAMEPointingAtItselfIsNotAnAnswer(t *testing.T) {
	sys := &fakeSystem{cname: map[string]string{"flat.example": "flat.example."}}
	h := newTestHandler(t, sys, false)
	hdr, answers := reply(t, ask(t, h, "flat.example.", dnsmessage.TypeCNAME))
	if hdr.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Errorf("self CNAME = %v with %d answers", hdr.RCode, len(answers))
	}
}

func TestUnsupportedTypeIsNotImplemented(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	hdr, _ := reply(t, ask(t, h, "example.", dnsmessage.TypeSOA))
	if hdr.RCode != dnsmessage.RCodeNotImplemented {
		t.Errorf("SOA = %v, want NOTIMP", hdr.RCode)
	}
}

func TestMalformedQueries(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	ctx := context.Background()

	if got := h.Answer(ctx, nil); got != nil {
		t.Errorf("empty query answered with %d bytes, want a silent drop", len(got))
	}
	if got := h.Answer(ctx, []byte{0x12}); got != nil {
		t.Errorf("truncated header answered, want a silent drop")
	}
	// A well-formed header whose question section is cut short.
	q := query(t, "example.com.", dnsmessage.TypeA, dnsmessage.ClassINET, 0)
	hdr, _ := reply(t, h.Answer(ctx, q[:len(q)-4]))
	if hdr.RCode != dnsmessage.RCodeFormatError {
		t.Errorf("truncated question = %v, want FORMERR", hdr.RCode)
	}
	// A response is not a query; bouncing it back invites a loop.
	if got := h.Answer(ctx, h.Answer(ctx, q)); got != nil {
		t.Errorf("a response was answered, want a silent drop")
	}
}

func TestNonInternetClassIsRefused(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	q := query(t, "example.com.", dnsmessage.TypeA, dnsmessage.ClassCHAOS, 0)
	hdr, _ := reply(t, h.Answer(context.Background(), q))
	if hdr.RCode != dnsmessage.RCodeRefused {
		t.Errorf("CHAOS class = %v, want REFUSED", hdr.RCode)
	}
}

// The reply must fit the requester's UDP budget: over it, answers are
// dropped and TC tells the requester to come back over TCP.
func TestTruncationAndEDNS(t *testing.T) {
	var many []net.IP
	for i := 0; i < 200; i++ {
		many = append(many, net.IPv4(10, 0, byte(i/256), byte(i%256)))
	}
	sys := &fakeSystem{ips: map[string][]net.IP{"big.example": many}}
	h := newTestHandler(t, sys, false)

	small := h.Answer(context.Background(), query(t, "big.example.", dnsmessage.TypeA, dnsmessage.ClassINET, 0))
	if len(small) > MinUDPSize {
		t.Errorf("reply without EDNS is %d bytes, over the %d-byte limit", len(small), MinUDPSize)
	}
	hdr, answers := reply(t, small)
	if !hdr.Truncated || len(answers) != 0 {
		t.Errorf("truncated reply: TC=%v with %d answers", hdr.Truncated, len(answers))
	}

	big := h.Answer(context.Background(), query(t, "big.example.", dnsmessage.TypeA, dnsmessage.ClassINET, 4096))
	hdr, answers = reply(t, big)
	if hdr.Truncated || len(answers) == 0 {
		t.Errorf("EDNS reply: TC=%v with %d answers", hdr.Truncated, len(answers))
	}
	// TCP has no such budget: every answer fits.
	tcp := h.answerTCP(context.Background(), query(t, "big.example.", dnsmessage.TypeA, dnsmessage.ClassINET, 0))
	if _, answers = reply(t, tcp); len(answers) != len(many) {
		t.Errorf("tcp reply carried %d answers, want %d", len(answers), len(many))
	}
}

// The reply must echo the request's ID and question, or no client will
// match it to anything.
func TestReplyEchoesRequest(t *testing.T) {
	sys := &fakeSystem{ips: map[string][]net.IP{"a.example": {net.ParseIP("192.0.2.1")}}}
	h := newTestHandler(t, sys, false)
	msg := ask(t, h, "a.example.", dnsmessage.TypeA)
	var p dnsmessage.Parser
	hdr, err := p.Start(msg)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.ID != 0x1234 || !hdr.Response || !hdr.RecursionAvailable || !hdr.RecursionDesired {
		t.Errorf("header = %+v", hdr)
	}
	q, err := p.Question()
	if err != nil || q.Name.String() != "a.example." || q.Type != dnsmessage.TypeA {
		t.Errorf("question = %+v, %v", q, err)
	}
}

func TestArpaToIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2.127.168.192.in-addr.arpa.", "192.168.127.2", true},
		{"9.0.168.192.in-addr.arpa", "192.168.0.9", true},
		{"1.0.168.192.in-addr.arpa.extra.", "", false},
		{"168.192.in-addr.arpa.", "", false},
		{"example.com.", "", false},
		{"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa.", "2001:db8::1", true},
		{"x.0.0.0.ip6.arpa.", "", false},
	}
	for _, c := range cases {
		got, ok := arpaToIP(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("arpaToIP(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestAliasTableCanBeReplaced(t *testing.T) {
	h := newTestHandler(t, &fakeSystem{}, false)
	h.SetAliases(map[string]netip.Addr{"newname.local": hostAlias})
	if got := addresses(t, ask(t, h, "newname.local.", dnsmessage.TypeA)); len(got) != 1 || got[0] != hostAlias.String() {
		t.Errorf("new alias = %v", got)
	}
	// The old table is gone: the name is forwarded and the fake knows
	// nothing about it.
	if hdr, _ := reply(t, ask(t, h, "darwin.local.", dnsmessage.TypeA)); hdr.RCode != dnsmessage.RCodeNameError {
		t.Errorf("replaced alias still answered locally: %v", hdr.RCode)
	}
}

func TestDefaultAliases(t *testing.T) {
	got := DefaultAliases(hostAlias, gatewayIP, []string{"Darwin", "darwin.local"})
	for _, want := range []string{"host.containers.internal.", "host.docker.internal.", "darwin.", "darwin.local."} {
		if got[want] != hostAlias {
			t.Errorf("%s = %v, want %v", want, got[want], hostAlias)
		}
	}
	if got["gateway.containers.internal."] != gatewayIP {
		t.Errorf("gateway alias = %v", got["gateway.containers.internal."])
	}
	if len(DefaultAliases(netip.Addr{}, netip.Addr{}, nil)) != 0 {
		t.Error("an invalid alias address must produce no aliases")
	}
}

func TestSplitTXT(t *testing.T) {
	long := strings.Repeat("x", 600)
	parts := splitTXT(long)
	if len(parts) != 3 || len(parts[0]) != 255 || len(parts[2]) != 90 {
		t.Errorf("splitTXT produced %d parts of %d bytes", len(parts), len(parts[0]))
	}
}
