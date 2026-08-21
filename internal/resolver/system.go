package resolver

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The host resolver is reached through Go's cgo resolver: net.Resolver with
// PreferGo false calls libSystem's getaddrinfo(3), which goes through
// mDNSResponder and therefore honours everything "scutil --dns" shows —
// per-domain and interface-scoped nameservers (VPN split horizon), the
// search list, /etc/hosts and .local multicast discovery. The pure-Go
// resolver reads /etc/resolv.conf instead, which on macOS is a stale
// projection of the primary resolver only: it resolves public names and
// silently fails on everything the host knows privately.
//
// That difference is measurable, and it is the reason HostResolver
// (build-tagged, see hostresolver_darwin.go) exists. On darwin the
// libSystem path is compiled whatever CGO_ENABLED says, so the flag that
// would lose it is "-tags netgo" (or GODEBUG=netdns=go at run time): a
// build without it keeps working for public names while losing exactly the
// parity this package is for, so "jm doctor" reports it rather than letting
// it pass.

// lookupTimeout bounds one host lookup so a wedged VPN resolver cannot pin
// a guest query open forever; the guest's own resolver retries.
const lookupTimeout = 10 * time.Second

type cgoSystem struct{ r *net.Resolver }

// NewSystem returns the host operating system's resolver.
func NewSystem() System { return cgoSystem{r: &net.Resolver{PreferGo: false}} }

func (s cgoSystem) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	addrs, err := s.r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

func (s cgoSystem) LookupCNAME(ctx context.Context, host string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return s.r.LookupCNAME(ctx, host)
}

func (s cgoSystem) LookupPTR(ctx context.Context, addr string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return s.r.LookupAddr(ctx, addr)
}

func (s cgoSystem) LookupTXT(ctx context.Context, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return s.r.LookupTXT(ctx, name)
}

func (s cgoSystem) LookupSRV(ctx context.Context, name string) ([]*net.SRV, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	_, srvs, err := s.r.LookupSRV(ctx, "", "", name)
	return srvs, err
}

func (s cgoSystem) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return s.r.LookupMX(ctx, name)
}

func (s cgoSystem) LookupNS(ctx context.Context, name string) ([]*net.NS, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return s.r.LookupNS(ctx, name)
}

// Search-list limits taken from resolv.conf(5) on both systems: at most six
// domains, at most 256 characters in total.
const (
	maxSearchDomains = 6
	maxSearchLength  = 256
)

// SearchDomains returns the host's effective DNS search list, most specific
// first, as "scutil --dns" reports it: every resolver's search domains, in
// the order macOS lists them, de-duplicated. It is host state, not a jm
// setting, and is re-read rather than cached (ADR 0008).
func SearchDomains(ctx context.Context) []string {
	out, err := runCommand(ctx, "scutil", "--dns")
	if err != nil {
		return nil
	}
	return ParseSearchDomains(out)
}

// ParseSearchDomains extracts the search list from "scutil --dns" output.
func ParseSearchDomains(out string) []string {
	var domains []string
	seen := map[string]bool{}
	total := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "search domain[") {
			continue
		}
		_, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		d := strings.Trim(strings.TrimSpace(value), ".")
		if d == "" || seen[strings.ToLower(d)] || !plausibleDomain(d) {
			continue
		}
		if len(domains) >= maxSearchDomains || total+len(d)+1 > maxSearchLength {
			break
		}
		seen[strings.ToLower(d)] = true
		domains = append(domains, d)
		total += len(d) + 1
	}
	return domains
}

// plausibleDomain rejects anything that cannot be a domain in a resolver
// file: the value goes into the guest's /etc/resolv.conf, so a surprising
// character in host configuration must not turn into a broken guest.
func plausibleDomain(d string) bool {
	if len(d) > 253 {
		return false
	}
	for _, r := range d {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

// HostNames returns the names the host answers to for itself: its kernel
// hostname and its Bonjour name with the .local suffix. They are answered
// locally (ADR 0008) because the host resolver maps them to the host's own
// addresses, loopback included, which inside the guest would mean the guest.
func HostNames(ctx context.Context) []string {
	var names []string
	add := func(n string) {
		n = strings.Trim(strings.TrimSpace(n), ".")
		if n == "" || !plausibleDomain(n) {
			return
		}
		for _, have := range names {
			if strings.EqualFold(have, n) {
				return
			}
		}
		names = append(names, n)
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	if local, err := runCommand(ctx, "scutil", "--get", "LocalHostName"); err == nil {
		local = strings.TrimSpace(local)
		add(local)
		if local != "" {
			add(local + ".local")
		}
	}
	// A kernel hostname of "darwin" also answers as "darwin.local".
	for _, n := range append([]string(nil), names...) {
		if !strings.Contains(n, ".") {
			add(n + ".local")
		}
	}
	return names
}

// runCommand is exec.CommandContext plus a timeout, replaced in tests.
var runCommand = func(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}
