package resolver

import (
	"strings"
	"testing"
)

func goodGuest() GuestConfig {
	return GuestConfig{
		UpstreamIP:   "192.168.127.254",
		UpstreamPort: 53535,
		Nameserver:   "192.168.127.2",
		HostAlias:    "192.168.127.254",
		Search:       []string{"corp.example", "vpn.example"},
	}
}

func TestGuestConfigValidate(t *testing.T) {
	if err := goodGuest().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := []GuestConfig{
		{UpstreamIP: "not-an-ip", UpstreamPort: 53535, Nameserver: "192.168.127.2"},
		{UpstreamIP: "192.168.127.254", UpstreamPort: 0, Nameserver: "192.168.127.2"},
		{UpstreamIP: "192.168.127.254", UpstreamPort: 70000, Nameserver: "192.168.127.2"},
		{UpstreamIP: "192.168.127.254", UpstreamPort: 53535, Nameserver: ""},
		{UpstreamIP: "192.168.127.254", UpstreamPort: 53535, Nameserver: "192.168.127.2"},
		// A search domain must not be able to smuggle anything into a
		// configuration file in the guest.
		{UpstreamIP: "192.168.127.254", UpstreamPort: 53535, Nameserver: "192.168.127.2",
			HostAlias: "192.168.127.254", Search: []string{"evil\nnameserver 8.8.8.8"}},
	}
	for i, g := range bad {
		if err := g.Validate(); err == nil {
			t.Errorf("case %d: %+v was accepted", i, g)
		}
		if _, err := g.Script(); err == nil {
			t.Errorf("case %d: Script accepted an invalid config", i)
		}
	}
}

func TestUnboundConfForwardsToTheHostResolver(t *testing.T) {
	conf := goodGuest().UnboundConf()
	for _, want := range []string{
		"forward-addr: 192.168.127.254@53535",
		"name: \".\"",
		// No cache: the host's answer is only true when it was given.
		"cache-max-ttl: 0",
		"cache-max-negative-ttl: 0",
		// No validator, and no built-in "this cannot exist" zones for
		// private, link-local or .local names.
		"module-config: \"iterator\"",
		"unblock-lan-zones: yes",
		"local-zone: \"local.\" nodefault",
		"local-zone: \"254.169.in-addr.arpa.\" nodefault",
		// RFC 6761 special-use TLDs unbound blackholes by default; ".test"
		// is the standard local-development TLD, so a host /etc/hosts entry
		// under it must reach the host resolver, not NXDOMAIN.
		"local-zone: \"test.\" nodefault",
		"local-zone: \"invalid.\" nodefault",
		"local-zone: \"home.arpa.\" nodefault",
		"local-zone: \"onion.\" nodefault",
		// Containers reach it on the guest's own address, not only on
		// loopback, and the reply must come from that same address.
		"interface: 127.0.0.1",
		"interface: 192.168.127.2",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("unbound.conf is missing %q:\n%s", want, conf)
		}
	}
}

// unbound's built-in loopback answer for "localhost" must stand: disabling
// it would send localhost to the host resolver, whose 127.0.0.1 the handler
// rewrites to the host alias — so "localhost" in the guest would mean the
// Mac. Tested on a live guest; do not "complete" the list with it.
func TestUnboundConfKeepsTheBuiltInLocalhostZone(t *testing.T) {
	if conf := goodGuest().UnboundConf(); strings.Contains(conf, "\"localhost.\"") {
		t.Errorf("unbound.conf disables the built-in localhost zone:\n%s", conf)
	}
}

func TestResolvConfHasOneNameserver(t *testing.T) {
	got := goodGuest().ResolvConf()
	if !strings.Contains(got, "search corp.example vpn.example\n") {
		t.Errorf("search line missing:\n%s", got)
	}
	n := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "nameserver ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("resolv.conf has %d nameservers, want exactly one:\n%s", n, got)
	}
	if !strings.Contains(got, "nameserver 192.168.127.2\n") {
		t.Errorf("wrong nameserver:\n%s", got)
	}
	// No search list at all is legal: the host may have none.
	g := goodGuest()
	g.Search = nil
	if strings.Contains(g.ResolvConf(), "search") {
		t.Errorf("empty search list produced a search line:\n%s", g.ResolvConf())
	}
}

// The script must be safe to run on every start: it rewrites nothing that
// has not changed, restarts nothing whose configuration has not changed,
// and only takes /etc/resolv.conf over once the forwarder answers.
func TestGuestScriptIsIdempotentAndOrdered(t *testing.T) {
	script, err := goodGuest().Script()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set -e",
		"cmp -s /var/unbound/unbound.conf.jm /var/unbound/unbound.conf",
		"cmp -s /etc/resolv.conf.jm /etc/resolv.conf",
		"sysrc local_unbound_enable=YES",
		"resolv_conf=\"/dev/null\"",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script is missing %q:\n%s", want, script)
		}
	}
	probeAt := strings.Index(script, "host -W 2 -t A host.containers.internal")
	resolvAt := strings.Index(script, "cat > /etc/resolv.conf.jm")
	restartAt := strings.Index(script, "service local_unbound start")
	if probeAt < 0 || resolvAt < 0 || restartAt < 0 {
		t.Fatalf("script does not have the expected steps:\n%s", script)
	}
	if !(restartAt < probeAt && probeAt < resolvAt) {
		t.Errorf("wrong order: restart at %d, probe at %d, resolv.conf at %d:\n%s", restartAt, probeAt, resolvAt, script)
	}
}

// The engine writes host.containers.internal into every container's hosts
// file, where it wins over DNS; it must name the user's computer, not the
// guest.
func TestContainersConfPointsTheAliasAtTheHost(t *testing.T) {
	got := goodGuest().ContainersConf()
	if !strings.Contains(got, "[containers]") || !strings.Contains(got, `host_containers_internal_ip = "192.168.127.254"`) {
		t.Errorf("containers drop-in:\n%s", got)
	}
	script, err := goodGuest().Script()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "/usr/local/etc/containers/containers.conf.d/50-jailmachine-dns.conf") {
		t.Errorf("script does not install the drop-in:\n%s", script)
	}
	if !strings.Contains(script, "service podman_service restart") {
		t.Errorf("script does not restart the engine when the drop-in changes:\n%s", script)
	}
}

func TestVerifyCommandQuotesItsArguments(t *testing.T) {
	got := VerifyCommand("host.containers.internal", "192.168.127.2")
	if !strings.Contains(got, "'host.containers.internal'") || !strings.Contains(got, "'192.168.127.2'") {
		t.Errorf("VerifyCommand = %q", got)
	}
	if evil := VerifyCommand("a'; rm -rf /; '", "1.2.3.4"); strings.Contains(evil, "; rm -rf /; ") &&
		!strings.Contains(evil, `'\''`) {
		t.Errorf("VerifyCommand did not quote its argument: %q", evil)
	}
}
