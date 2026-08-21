package resolver

import (
	"bufio"
	"context"
	"net/netip"
	"os"
	"strings"
)

// Asserting parity at run time (ADR 0008).
//
// A jm that lost the host resolver keeps answering public names, so neither
// "the resolver is alive" nor "the alias came back" is evidence of parity:
// the aliases are answered from the handler's own table and never reach the
// host at all. Two things are therefore readable from outside the resolver
// process, and "jm doctor" asserts both:
//
//   - StatusName, a reserved name the resolver answers *about itself*, so
//     the resolution path of the process actually serving the guest can be
//     asserted over the wire. A build with "-tags netgo" and a process
//     started with GODEBUG=netdns=go both keep public names working while
//     losing every scoped (VPN), /etc/hosts and .local name, and neither is
//     visible in a compile-time constant read by some other process.
//   - ParityProbe, a name the alias table does not hold and that only this
//     host can answer, so what the resolver hands back can be compared with
//     what the host itself resolves — the address, not merely that an
//     answer arrived.

// StatusName is the reserved name the resolver answers about itself. It
// sits under the same .internal domain as the aliases, which no public zone
// serves.
const StatusName = "resolver.jailmachine.internal"

// StatusPrefix labels the value in StatusName's TXT record, so the answer
// is self-describing on the wire.
const StatusPrefix = "mode="

// Resolution paths reported by StatusName.
const (
	// ModeHost is the host operating system's own resolver (getaddrinfo
	// through libSystem on darwin): scoped resolvers, /etc/hosts and
	// .local names all apply, which is what parity means.
	ModeHost = "host"
	// ModeGo is Go's own DNS client, which sees none of them.
	ModeGo = "go"
)

// statusFQDN is StatusName as the handler compares names.
var statusFQDN = canonical(StatusName)

// SystemMode reports how this process resolves names. It is deliberately a
// run-time answer: the build tag is only half of it, because
// GODEBUG=netdns=go gives the libSystem path up in a build that has it.
func SystemMode() string {
	if !HostResolver {
		return ModeGo
	}
	if netdnsSetting(os.Getenv("GODEBUG")) == "go" {
		return ModeGo
	}
	return ModeHost
}

// netdnsSetting extracts the resolver named by a GODEBUG string, the way
// the standard library reads it: the last "netdns=" wins, and a "+" suffix
// ("netdns=go+2") only turns its debug output on.
func netdnsSetting(godebug string) string {
	value := ""
	for _, field := range strings.Split(godebug, ",") {
		k, v, ok := strings.Cut(field, "=")
		if ok && strings.TrimSpace(k) == "netdns" {
			value = strings.TrimSpace(v)
		}
	}
	if i := strings.IndexByte(value, '+'); i >= 0 {
		value = value[:i]
	}
	return value
}

// ParseStatus reads the resolution path out of StatusName's TXT records.
func ParseStatus(txt []string) (string, bool) {
	for _, t := range txt {
		if mode, ok := strings.CutPrefix(strings.TrimSpace(t), StatusPrefix); ok {
			return mode, true
		}
	}
	return "", false
}

// HostsFile is the host's static host table, where a name only this machine
// can answer is found without inventing one. It is a variable so tests can
// point it elsewhere.
var HostsFile = "/etc/hosts"

// maxProbeCandidates bounds the work of picking a probe: each candidate
// costs one host lookup, and the first usable one wins.
const maxProbeCandidates = 8

// ParityProbe picks a name for asserting resolution parity and returns it
// with the addresses a resolver in parity must answer it with: the host's
// own answer, mapped the way a forwarded answer is mapped (a loopback
// address on the host is the host alias in the guest).
//
// The name comes from the host's static host table and is deliberately one
// the alias table does not hold: an alias round trip proves the alias
// table, not that anything reached the host resolver (ADR 0008). ok is
// false when this host offers no such name, which is not a failure — the
// caller then has nothing to compare and must say so rather than pass.
func ParityProbe(ctx context.Context, sys System, hostAlias netip.Addr, aliases map[string]netip.Addr) (name string, want []netip.Addr, ok bool) {
	if sys == nil {
		sys = NewSystem()
	}
	for _, candidate := range hostsFileNames(HostsFile) {
		if _, isAlias := aliases[canonical(candidate)]; isAlias {
			continue
		}
		ips, err := sys.LookupIP(ctx, candidate)
		if err != nil {
			continue
		}
		addrs := GuestAddrs(ips, hostAlias, false)
		if len(addrs) == 0 {
			continue
		}
		// A name whose only address is the host alias — a loopback entry,
		// rewritten — proves the lookup happened but not much else, so it
		// is kept only as a fallback: an address of its own distinguishes
		// a real host answer from anything jm could have invented.
		if !onlyHostAlias(addrs, hostAlias) {
			return candidate, addrs, true
		}
		if name == "" {
			name, want = candidate, addrs
		}
	}
	return name, want, name != ""
}

func onlyHostAlias(addrs []netip.Addr, hostAlias netip.Addr) bool {
	for _, a := range addrs {
		if a != hostAlias {
			return false
		}
	}
	return true
}

// hostsFileNames returns the names a hosts file maps, in file order, at
// most maxProbeCandidates of them.
func hostsFileNames(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var names []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() && len(names) < maxProbeCandidates {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// fields[0] is the address; the rest are names for it.
		for _, n := range fields[1:] {
			key := strings.ToLower(n)
			if seen[key] || !plausibleDomain(n) {
				continue
			}
			seen[key] = true
			names = append(names, n)
			if len(names) >= maxProbeCandidates {
				break
			}
		}
	}
	return names
}
