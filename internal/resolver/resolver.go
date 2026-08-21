// Package resolver is the host-side name-resolution component of ADR 0008.
//
// It answers DNS queries coming from the guest by asking the *host*
// operating system's own resolver — the same path a host application
// takes — so that every name the user's browser resolves resolves the same
// way inside the guest and inside containers: per-domain and
// interface-scoped resolvers, VPN split horizon, /etc/hosts overrides and
// multicast .local discovery all apply without jm modelling any of them.
//
// The component is deliberately small and pure: Handler.Answer maps one
// wire-format query to one wire-format reply through a System interface,
// so the whole policy (aliases, address families, error propagation) is
// unit-testable against a fake system resolver, with no network at all.
//
// What it does *not* do is fall back to a public resolver when the host
// resolver fails: on a split-horizon network that would answer an internal
// name with a public address, and a wrong answer is worse than no answer
// (ADR 0008).
package resolver

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

// UDP payload sizes. A reply larger than the requester's advertised size is
// truncated with TC set, and the requester retries over TCP.
const (
	// MinUDPSize is the payload size assumed for a query without EDNS(0).
	MinUDPSize = 512
	// MaxUDPSize is the payload size we advertise in our own OPT record.
	MaxUDPSize = 4096
)

// answerTTL is the TTL of every record we synthesise. Parity is "at the
// time of asking" (ADR 0008): the host's answer may change with the next
// network event, so nothing downstream may cache it.
const answerTTL = 0

// System is the host operating system's resolver, injected so the handler
// can be tested without a network.
//
// Implementations must return errors verbatim; NewSystem's cgo-backed
// implementation returns *net.DNSError, whose IsNotFound distinguishes
// NXDOMAIN from a failure that must surface as SERVFAIL.
type System interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupPTR(ctx context.Context, name string) ([]string, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupSRV(ctx context.Context, name string) ([]*net.SRV, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupNS(ctx context.Context, name string) ([]*net.NS, error)
}

// Config describes one machine's resolver.
type Config struct {
	// HostAlias is the address that reaches the host from inside the guest
	// network (gvproxy NATs 192.168.127.254 to the host's loopback). It is
	// what host.containers.internal answers, and what a loopback address
	// coming out of the host resolver is rewritten to: 127.0.0.1 means
	// "this machine" on the host and would mean the guest in the guest.
	HostAlias netip.Addr
	// Gateway is the guest network's gateway, answered for the
	// gateway.*.internal names.
	Gateway netip.Addr
	// Aliases are names answered locally and never forwarded, keyed by
	// fully-qualified lower-case name (a trailing dot is optional).
	// DefaultAliases builds the standard set.
	Aliases map[string]netip.Addr
	// AllowIPv6 lets AAAA answers through. The gvproxy network is IPv4
	// only, so an AAAA address would be an address the guest cannot reach
	// (ADR 0008): with AllowIPv6 false a name with IPv6 addresses only
	// yields an empty answer, not an unroutable one.
	AllowIPv6 bool
	// System is the host resolver; NewSystem() when nil.
	System System
	// Mode is what StatusName reports about this process's resolution
	// path; SystemMode() when empty.
	Mode string
	// Log receives one line per failed query; discarded when nil.
	Log *log.Logger
}

// Handler answers DNS queries from a Config. It is safe for concurrent use,
// and its alias table can be replaced while it serves: the host's own names
// change when the user renames the Mac or joins a network.
type Handler struct {
	cfg     Config
	mu      sync.RWMutex
	aliases map[string]netip.Addr
}

// NewHandler normalises cfg and returns the handler.
func NewHandler(cfg Config) *Handler {
	if cfg.System == nil {
		cfg.System = NewSystem()
	}
	if cfg.Mode == "" {
		cfg.Mode = SystemMode()
	}
	h := &Handler{cfg: cfg}
	h.SetAliases(cfg.Aliases)
	return h
}

// SetAliases replaces the table of locally-answered names.
func (h *Handler) SetAliases(aliases map[string]netip.Addr) {
	normalised := make(map[string]netip.Addr, len(aliases))
	for name, ip := range aliases {
		if !ip.IsValid() {
			continue
		}
		normalised[canonical(name)] = ip
	}
	h.mu.Lock()
	h.aliases = normalised
	h.mu.Unlock()
}

// Aliases returns a copy of the normalised alias table.
func (h *Handler) Aliases() map[string]netip.Addr {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]netip.Addr, len(h.aliases))
	for k, v := range h.aliases {
		out[k] = v
	}
	return out
}

// alias looks one name up in the table.
func (h *Handler) alias(name string) (netip.Addr, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ip, ok := h.aliases[name]
	return ip, ok
}

// canonical lower-cases a name and gives it exactly one trailing dot.
func canonical(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "."
	}
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return name
}

// DefaultAliases is the alias table of ADR 0008: the host alias under both
// the podman and the docker names, the gateway under both, and every name
// the host answers to for itself (its hostname and its .local name), which
// must point at the host and not at something inside the guest.
func DefaultAliases(hostAlias, gateway netip.Addr, hostNames []string) map[string]netip.Addr {
	out := map[string]netip.Addr{}
	if hostAlias.IsValid() {
		out["host.containers.internal."] = hostAlias
		out["host.docker.internal."] = hostAlias
		out["hostmachine.internal."] = hostAlias
		for _, n := range hostNames {
			if n = canonical(n); n != "." {
				out[n] = hostAlias
			}
		}
	}
	if gateway.IsValid() {
		out["gateway.containers.internal."] = gateway
		out["gateway.docker.internal."] = gateway
	}
	return out
}

// Answer maps one wire-format query to one wire-format reply. It returns
// nil when the query is too broken to reply to at all (no usable header),
// which is the only case a server may drop silently.
func (h *Handler) Answer(ctx context.Context, query []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil
	}
	if hdr.Response {
		// A reply sent to a server is not a query; never bounce it back.
		return nil
	}
	q, qerr := p.Question()
	if qerr != nil {
		return h.build(hdr, nil, dnsmessage.RCodeFormatError, nil, edns{})
	}
	// Exactly one question: a second one has no unambiguous answer and no
	// resolver in the wild sends one.
	if _, err := p.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return h.build(hdr, nil, dnsmessage.RCodeFormatError, nil, h.edns(&p))
	}
	e := h.edns(&p)
	if q.Class != dnsmessage.ClassINET && q.Class != dnsmessage.ClassANY {
		return h.build(hdr, &q, dnsmessage.RCodeRefused, nil, e)
	}
	if hdr.OpCode != 0 {
		return h.build(hdr, &q, dnsmessage.RCodeNotImplemented, nil, e)
	}
	answers, rcode := h.resolve(ctx, q)
	return h.build(hdr, &q, rcode, answers, e)
}

// resolve produces the answer section for one question.
func (h *Handler) resolve(ctx context.Context, q dnsmessage.Question) ([]dnsmessage.Resource, dnsmessage.RCode) {
	name := canonical(q.Name.String())
	if name == statusFQDN {
		return h.statusAnswer(q), dnsmessage.RCodeSuccess
	}
	if ip, ok := h.alias(name); ok {
		return h.aliasAnswer(q, ip), dnsmessage.RCodeSuccess
	}
	host := strings.TrimSuffix(name, ".")
	switch q.Type {
	case dnsmessage.TypeA:
		return h.addressAnswer(ctx, q, host, false)
	case dnsmessage.TypeAAAA:
		return h.addressAnswer(ctx, q, host, true)
	case dnsmessage.TypeCNAME:
		cname, err := h.cfg.System.LookupCNAME(ctx, host)
		if err != nil {
			return nil, h.fail(q, err)
		}
		target, ok := toName(cname)
		if !ok || canonical(cname) == name {
			// getaddrinfo/res_search echo the query when there is no
			// CNAME; an answer pointing at itself is not one.
			return nil, dnsmessage.RCodeSuccess
		}
		return []dnsmessage.Resource{{
			Header: rrHeader(q, dnsmessage.TypeCNAME),
			Body:   &dnsmessage.CNAMEResource{CNAME: target},
		}}, dnsmessage.RCodeSuccess
	case dnsmessage.TypePTR:
		// The host resolution API takes an address, not a reverse name.
		addr, ok := arpaToIP(name)
		if !ok {
			return nil, dnsmessage.RCodeSuccess
		}
		names, err := h.cfg.System.LookupPTR(ctx, addr)
		if err != nil {
			return nil, h.fail(q, err)
		}
		var out []dnsmessage.Resource
		for _, n := range names {
			target, ok := toName(n)
			if !ok {
				continue
			}
			out = append(out, dnsmessage.Resource{
				Header: rrHeader(q, dnsmessage.TypePTR),
				Body:   &dnsmessage.PTRResource{PTR: target},
			})
		}
		return out, dnsmessage.RCodeSuccess
	case dnsmessage.TypeTXT:
		txts, err := h.cfg.System.LookupTXT(ctx, host)
		if err != nil {
			return nil, h.fail(q, err)
		}
		var out []dnsmessage.Resource
		for _, t := range txts {
			out = append(out, dnsmessage.Resource{
				Header: rrHeader(q, dnsmessage.TypeTXT),
				Body:   &dnsmessage.TXTResource{TXT: splitTXT(t)},
			})
		}
		return out, dnsmessage.RCodeSuccess
	case dnsmessage.TypeSRV:
		srvs, err := h.cfg.System.LookupSRV(ctx, host)
		if err != nil {
			return nil, h.fail(q, err)
		}
		var out []dnsmessage.Resource
		for _, s := range srvs {
			target, ok := toName(s.Target)
			if !ok {
				continue
			}
			out = append(out, dnsmessage.Resource{
				Header: rrHeader(q, dnsmessage.TypeSRV),
				Body: &dnsmessage.SRVResource{
					Priority: s.Priority, Weight: s.Weight, Port: s.Port, Target: target,
				},
			})
		}
		return out, dnsmessage.RCodeSuccess
	case dnsmessage.TypeMX:
		mxs, err := h.cfg.System.LookupMX(ctx, host)
		if err != nil {
			return nil, h.fail(q, err)
		}
		var out []dnsmessage.Resource
		for _, mx := range mxs {
			target, ok := toName(mx.Host)
			if !ok {
				continue
			}
			out = append(out, dnsmessage.Resource{
				Header: rrHeader(q, dnsmessage.TypeMX),
				Body:   &dnsmessage.MXResource{Pref: mx.Pref, MX: target},
			})
		}
		return out, dnsmessage.RCodeSuccess
	case dnsmessage.TypeNS:
		nss, err := h.cfg.System.LookupNS(ctx, host)
		if err != nil {
			return nil, h.fail(q, err)
		}
		var out []dnsmessage.Resource
		for _, ns := range nss {
			target, ok := toName(ns.Host)
			if !ok {
				continue
			}
			out = append(out, dnsmessage.Resource{
				Header: rrHeader(q, dnsmessage.TypeNS),
				Body:   &dnsmessage.NSResource{NS: target},
			})
		}
		return out, dnsmessage.RCodeSuccess
	default:
		// The host resolution API offers no way to ask for an arbitrary
		// record type, so say so rather than inventing an empty answer
		// that would read as "this name has no such record".
		return nil, dnsmessage.RCodeNotImplemented
	}
}

// statusAnswer describes the resolver itself. It is how "jm doctor" learns,
// over the wire and therefore about the process actually serving the guest,
// whether queries really go through the host operating system's resolver:
// one that does not keeps answering public names while losing every scoped,
// hosts-file and .local name, which is the regression ADR 0008 refuses to
// let pass unseen. Only TXT: the name is a report, not an address.
func (h *Handler) statusAnswer(q dnsmessage.Question) []dnsmessage.Resource {
	if q.Type != dnsmessage.TypeTXT && q.Type != dnsmessage.TypeALL {
		return nil
	}
	return []dnsmessage.Resource{{
		Header: rrHeader(q, dnsmessage.TypeTXT),
		Body:   &dnsmessage.TXTResource{TXT: []string{StatusPrefix + h.cfg.Mode}},
	}}
}

// aliasAnswer answers a locally-held name. Aliases are IPv4 addresses on
// the provider's network, so an AAAA (or any other type) for one is an
// empty NOERROR: the name exists, that record type does not.
func (h *Handler) aliasAnswer(q dnsmessage.Question, ip netip.Addr) []dnsmessage.Resource {
	if q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeALL {
		return nil
	}
	if !ip.Is4() {
		return nil
	}
	return []dnsmessage.Resource{{
		Header: rrHeader(q, dnsmessage.TypeA),
		Body:   &dnsmessage.AResource{A: ip.As4()},
	}}
}

// addressAnswer asks the host resolver and keeps the addresses the guest
// can actually route to.
func (h *Handler) addressAnswer(ctx context.Context, q dnsmessage.Question, host string, wantV6 bool) ([]dnsmessage.Resource, dnsmessage.RCode) {
	ips, err := h.cfg.System.LookupIP(ctx, host)
	if err != nil {
		return nil, h.fail(q, err)
	}
	if wantV6 && !h.cfg.AllowIPv6 {
		// The provider cannot route IPv6: the name exists, but there is
		// no address of that family we may hand out (ADR 0008).
		return nil, dnsmessage.RCodeSuccess
	}
	var out []dnsmessage.Resource
	for _, a := range GuestAddrs(ips, h.cfg.HostAlias, wantV6) {
		if wantV6 {
			out = append(out, dnsmessage.Resource{
				Header: rrHeader(q, dnsmessage.TypeAAAA),
				Body:   &dnsmessage.AAAAResource{AAAA: a.As16()},
			})
			continue
		}
		out = append(out, dnsmessage.Resource{
			Header: rrHeader(q, dnsmessage.TypeA),
			Body:   &dnsmessage.AResource{A: a.As4()},
		})
	}
	return out, dnsmessage.RCodeSuccess
}

// GuestAddrs keeps, of the addresses the host resolver returned, the ones a
// guest may be handed: the requested family, nothing the guest cannot route
// to, and the host's loopback rewritten to the host alias. It is exported
// because asserting parity means comparing an answer against the host's own
// (ADR 0008), and the comparison has to apply the same rules the answer did
// rather than a second copy of them.
func GuestAddrs(ips []net.IP, hostAlias netip.Addr, wantV6 bool) []netip.Addr {
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, ip := range ips {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		a = a.Unmap()
		if a.Is4() == wantV6 {
			continue
		}
		if a.IsLinkLocalUnicast() || a.IsMulticast() {
			continue
		}
		if a.IsUnspecified() {
			// "0.0.0.0 <name>" (and "::") is how a hosts file blocks a
			// name, and macOS getaddrinfo hands it back verbatim. It is
			// not an address anything can reach, on the host or in the
			// guest: drop it, so a blocked name is an empty NOERROR here
			// as it is there. Rewriting it to the host alias would point
			// every blocked domain at a service on the Mac (ADR 0008).
			continue
		}
		if a.IsLoopback() {
			// A host service on the loopback is reachable from the guest
			// at the host alias; the guest's own loopback is not it.
			if !hostAlias.IsValid() || wantV6 {
				continue
			}
			a = hostAlias
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// fail turns a host resolver error into the response code the guest must
// see: "no such name" stays NXDOMAIN, everything else is a server failure.
// Nothing is ever answered from another source (ADR 0008).
func (h *Handler) fail(q dnsmessage.Question, err error) dnsmessage.RCode {
	var derr *net.DNSError
	if errors.As(err, &derr) && derr.IsNotFound {
		return dnsmessage.RCodeNameError
	}
	if h.cfg.Log != nil {
		h.cfg.Log.Printf("%s %s: %v", q.Type, q.Name.String(), err)
	}
	return dnsmessage.RCodeServerFailure
}

// edns holds what we learned from the query's OPT record.
type edns struct {
	present bool
	size    uint16
	dnssec  bool
}

// udpSize is the largest reply the requester will accept over UDP.
func (e edns) udpSize() int {
	if !e.present || e.size < MinUDPSize {
		return MinUDPSize
	}
	if int(e.size) > MaxUDPSize {
		return MaxUDPSize
	}
	return int(e.size)
}

// edns scans the query's additional section for an OPT record. A malformed
// tail is simply "no EDNS": the question has already been read.
func (h *Handler) edns(p *dnsmessage.Parser) edns {
	if p.SkipAllQuestions() != nil || p.SkipAllAnswers() != nil || p.SkipAllAuthorities() != nil {
		return edns{}
	}
	for {
		hdr, err := p.AdditionalHeader()
		if err != nil {
			return edns{}
		}
		if hdr.Type == dnsmessage.TypeOPT {
			return edns{present: true, size: uint16(hdr.Class), dnssec: hdr.DNSSECAllowed()}
		}
		if p.SkipAdditional() != nil {
			return edns{}
		}
	}
}

// build renders the reply, truncating it (TC set, answers dropped) when it
// does not fit the requester's UDP budget. TCP callers pass a query whose
// EDNS size is irrelevant and re-render with maxTCPReply.
func (h *Handler) build(hdr dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, answers []dnsmessage.Resource, e edns) []byte {
	msg, err := render(hdr, q, rcode, answers, e, false)
	if err != nil {
		msg, err = render(hdr, q, dnsmessage.RCodeServerFailure, nil, e, false)
		if err != nil {
			return nil
		}
	}
	if len(msg) <= e.udpSize() {
		return msg
	}
	if truncated, err := render(hdr, q, rcode, nil, e, true); err == nil {
		return truncated
	}
	return msg
}

// BuildTCP renders the reply without a UDP size limit; used by the TCP
// listener, where a 64 KiB message is legal.
func (h *Handler) answerTCP(ctx context.Context, query []byte) []byte {
	var p dnsmessage.Parser
	hdr, err := p.Start(query)
	if err != nil {
		return nil
	}
	if hdr.Response {
		return nil
	}
	q, qerr := p.Question()
	if qerr != nil {
		return h.build(hdr, nil, dnsmessage.RCodeFormatError, nil, edns{present: true, size: MaxUDPSize})
	}
	if _, err := p.Question(); !errors.Is(err, dnsmessage.ErrSectionDone) {
		return h.build(hdr, nil, dnsmessage.RCodeFormatError, nil, edns{present: true, size: MaxUDPSize})
	}
	e := h.edns(&p)
	if q.Class != dnsmessage.ClassINET && q.Class != dnsmessage.ClassANY {
		return renderOrNil(hdr, &q, dnsmessage.RCodeRefused, nil, e)
	}
	if hdr.OpCode != 0 {
		return renderOrNil(hdr, &q, dnsmessage.RCodeNotImplemented, nil, e)
	}
	answers, rcode := h.resolve(ctx, q)
	return renderOrNil(hdr, &q, rcode, answers, e)
}

func renderOrNil(hdr dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, answers []dnsmessage.Resource, e edns) []byte {
	msg, err := render(hdr, q, rcode, answers, e, false)
	if err != nil {
		if msg, err = render(hdr, q, dnsmessage.RCodeServerFailure, nil, e, false); err != nil {
			return nil
		}
	}
	return msg
}

func render(hdr dnsmessage.Header, q *dnsmessage.Question, rcode dnsmessage.RCode, answers []dnsmessage.Resource, e edns, truncated bool) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 1024), dnsmessage.Header{
		ID:                 hdr.ID,
		Response:           true,
		OpCode:             hdr.OpCode,
		RecursionDesired:   hdr.RecursionDesired,
		RecursionAvailable: true,
		Truncated:          truncated,
		RCode:              rcode,
	})
	b.EnableCompression()
	if q != nil {
		if err := b.StartQuestions(); err != nil {
			return nil, err
		}
		if err := b.Question(*q); err != nil {
			return nil, err
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	for _, a := range answers {
		if err := addResource(&b, a); err != nil {
			return nil, err
		}
	}
	if e.present {
		if err := b.StartAdditionals(); err != nil {
			return nil, err
		}
		var opt dnsmessage.ResourceHeader
		if err := opt.SetEDNS0(MaxUDPSize, dnsmessage.RCodeSuccess, e.dnssec); err != nil {
			return nil, err
		}
		if err := b.OPTResource(opt, dnsmessage.OPTResource{}); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

func addResource(b *dnsmessage.Builder, r dnsmessage.Resource) error {
	switch body := r.Body.(type) {
	case *dnsmessage.AResource:
		return b.AResource(r.Header, *body)
	case *dnsmessage.AAAAResource:
		return b.AAAAResource(r.Header, *body)
	case *dnsmessage.CNAMEResource:
		return b.CNAMEResource(r.Header, *body)
	case *dnsmessage.PTRResource:
		return b.PTRResource(r.Header, *body)
	case *dnsmessage.TXTResource:
		return b.TXTResource(r.Header, *body)
	case *dnsmessage.SRVResource:
		return b.SRVResource(r.Header, *body)
	case *dnsmessage.MXResource:
		return b.MXResource(r.Header, *body)
	case *dnsmessage.NSResource:
		return b.NSResource(r.Header, *body)
	default:
		return errors.New("resolver: unsupported record body")
	}
}

func rrHeader(q dnsmessage.Question, typ dnsmessage.Type) dnsmessage.ResourceHeader {
	return dnsmessage.ResourceHeader{Name: q.Name, Type: typ, Class: dnsmessage.ClassINET, TTL: answerTTL}
}

// toName converts a host name to a wire name, reporting failure instead of
// panicking on a name the host resolver returned but DNS cannot encode.
func toName(s string) (dnsmessage.Name, bool) {
	n, err := dnsmessage.NewName(canonical(s))
	if err != nil {
		return dnsmessage.Name{}, false
	}
	return n, true
}

// splitTXT chops a TXT string into the 255-byte character-strings the wire
// format is made of.
func splitTXT(s string) []string {
	const max = 255
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		out = append(out, s[:max])
		s = s[max:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

// arpaToIP turns a reverse-lookup name ("2.127.168.192.in-addr.arpa." or
// the ip6.arpa nibble form) into the address literal the host resolution
// API expects. It reports false for anything that is not a reverse name.
func arpaToIP(name string) (string, bool) {
	name = strings.TrimSuffix(canonical(name), ".")
	switch {
	case strings.HasSuffix(name, ".in-addr.arpa"):
		labels := strings.Split(strings.TrimSuffix(name, ".in-addr.arpa"), ".")
		if len(labels) != 4 {
			return "", false
		}
		slices.Reverse(labels)
		ip, err := netip.ParseAddr(strings.Join(labels, "."))
		if err != nil || !ip.Is4() {
			return "", false
		}
		return ip.String(), true
	case strings.HasSuffix(name, ".ip6.arpa"):
		labels := strings.Split(strings.TrimSuffix(name, ".ip6.arpa"), ".")
		if len(labels) != 32 {
			return "", false
		}
		slices.Reverse(labels)
		var sb strings.Builder
		for i, l := range labels {
			if len(l) != 1 {
				return "", false
			}
			if i > 0 && i%4 == 0 {
				sb.WriteByte(':')
			}
			sb.WriteString(l)
		}
		ip, err := netip.ParseAddr(sb.String())
		if err != nil || !ip.Is6() {
			return "", false
		}
		return ip.String(), true
	}
	return "", false
}
