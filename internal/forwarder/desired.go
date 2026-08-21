package forwarder

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// DefaultHostIP is the host address a published port binds to: every
// interface, as "docker run -p 8080:80" does on Linux. The provider's
// listener is dual-stack, so 127.0.0.1, ::1, "localhost" and the machine's
// LAN address all reach the container.
const DefaultHostIP = "0.0.0.0"

// PublishAddrEnv overrides that address for people who do not want
// containers on the LAN: JM_PUBLISH_ADDR=127.0.0.1 keeps published ports on
// the host's loopback. It is a host-side choice; the guest is unaffected.
// "jm start" reads it and writes it onto the machine record, so that the
// address a detached forwarder is really binding is the one "jm inspect"
// and "jm ports" show, rather than ambient state of the shell that
// happened to boot the machine.
const PublishAddrEnv = "JM_PUBLISH_ADDR"

// ParsePublishAddr validates a publish address, returning its canonical
// form. An empty value means the default. A typo must be a usage error
// where it is typed, not a per-mapping expose failure minutes later.
func ParsePublishAddr(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	ip := net.ParseIP(v)
	if ip == nil {
		return "", fmt.Errorf("%q is not an IP address (%s publishes on every interface, %s on the loopback only)",
			v, DefaultHostIP, "127.0.0.1")
	}
	return ip.String(), nil
}

// HostIP resolves a machine's configured publish address, falling back to
// the default.
func HostIP(configured string) string {
	if v := strings.TrimSpace(configured); v != "" {
		return v
	}
	return DefaultHostIP
}

// psContainer is the subset of one "podman ps --format json" entry we use
// (podman 5.x/6.x shape).
type psContainer struct {
	Names  []string `json:"Names"`
	State  string   `json:"State"`
	Exited bool     `json:"Exited"`
	Ports  []psPort `json:"Ports"`
}

// psPort is podman's types.PortMapping: a range of Range ports starting at
// HostPort/ContainerPort.
type psPort struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Protocol      string `json:"protocol"`
	Range         uint16 `json:"range"`
}

// Desired turns "podman ps --format json" output into the sorted,
// deduplicated set of mappings the host should have. Each published host
// port is mapped to the same port on the guest (podman in the guest already
// maps it to the container port). Containers that are not running (ps -a
// output, exited entries) contribute nothing.
func Desired(psJSON []byte, guestIP, hostIP string) ([]netprov.Mapping, error) {
	out, _, err := Plan(psJSON, guestIP, hostIP)
	return out, err
}

// Plan is Desired plus the published ports that cannot be reached from
// the host: a host_ip that is neither a wildcard nor the guest's own
// address (typically "-p 127.0.0.1:8080:80") makes podman in the guest
// bind that address only, so the guest's loopback, not the host's, gets
// the port. Those come back as entries with Error set and no Remote, so
// "jm ports" can say why they are unreachable instead of listing them as
// ok.
func Plan(psJSON []byte, guestIP, hostIP string) ([]netprov.Mapping, []Entry, error) {
	hostIP = HostIP(hostIP)
	var cs []psContainer
	if len(psJSON) > 0 {
		if err := json.Unmarshal(psJSON, &cs); err != nil {
			return nil, nil, fmt.Errorf("parsing podman ps output: %w", err)
		}
	}
	seen := map[string]bool{}
	var out []netprov.Mapping
	var skipped []Entry
	for _, c := range cs {
		if c.Exited || (c.State != "" && c.State != "running" && c.State != "paused") {
			continue
		}
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			for _, mp := range expand(p, guestIP, hostIP) {
				k := key(mp)
				if seen[k] {
					continue
				}
				seen[k] = true
				if mp.Remote == "" {
					skipped = append(skipped, Entry{Proto: mp.Proto, Local: mp.Local,
						Error: unpublishableReason(p)})
					continue
				}
				out = append(out, mp)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	sort.Slice(skipped, func(i, j int) bool { return key(skipped[i].Mapping()) < key(skipped[j].Mapping()) })
	return out, skipped, nil
}

// publishable reports whether a port published in the guest can be reached
// at the guest's own address, which is the only way into it from the host.
//
// The engine in the guest turns "-p 8080:80" into redirect rules for each
// of the guest's addresses, so an unqualified publish is reachable. Every
// form that names a host address is not: an address the guest does not have
// (the host's loopback, a host LAN address) makes it bind that address
// inside the guest, and the literal wildcard 0.0.0.0 makes it write a
// redirect rule whose destination is 0.0.0.0 itself, which no packet ever
// matches. Both are guest-side facts; nothing the forwarder does on the
// host can reach such a container.
func publishable(hostIP, guestIP string) bool {
	return hostIP == "" || hostIP == guestIP
}

// unpublishableReason explains, in the words of the command the user typed,
// why a published port cannot be reached and what to type instead.
func unpublishableReason(p psPort) string {
	target := fmt.Sprintf("-p %d:%d", p.HostPort, p.ContainerPort)
	if p.Protocol != "" && p.Protocol != "tcp" {
		target += "/" + p.Protocol
	}
	if p.HostIP == "0.0.0.0" || p.HostIP == "::" {
		return "the guest engine redirects to " + p.HostIP + " itself, which never matches; publish with " + target + " (no host address)"
	}
	// The remedy has two halves and needs both: dropping the host address
	// is what makes the port reachable at all, and the machine's publish
	// address is what keeps the host side where the user asked for it.
	// Naming only the first would turn "keep this on the loopback" into
	// "put this on the LAN".
	return fmt.Sprintf("%s binds the guest's own %s, not the host's; publish with %s and bind the host side with 'jm set --publish-addr %s'",
		p.HostIP, addressWord(p.HostIP), target, p.HostIP)
}

// addressWord names the kind of address a host_ip is, so the message reads
// as an explanation rather than an echo.
func addressWord(ip string) string {
	if a := net.ParseIP(ip); a != nil && a.IsLoopback() {
		return "loopback"
	}
	return "address"
}

// expand turns one port mapping (with its range) into one Mapping per host
// port. Mappings whose host_ip is not publishable (see publishable) have an
// empty Remote.
func expand(p psPort, guestIP, hostIP string) []netprov.Mapping {
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	// A publishable port binds the host's chosen publish address; an
	// unpublishable one keeps the address the user asked for, so that
	// "jm ports" shows what they typed next to why it cannot work.
	local := p.HostIP
	remoteIP := guestIP
	if publishable(p.HostIP, guestIP) {
		local = hostIP
	} else {
		remoteIP = ""
	}
	n := int(p.Range)
	if n < 1 {
		n = 1
	}
	out := make([]netprov.Mapping, 0, n)
	for i := 0; i < n; i++ {
		port := int(p.HostPort) + i
		if port > 65535 {
			break
		}
		remote := ""
		if remoteIP != "" {
			remote = net.JoinHostPort(remoteIP, strconv.Itoa(port))
		}
		out = append(out, netprov.Mapping{
			Proto:  proto,
			Local:  net.JoinHostPort(local, strconv.Itoa(port)),
			Remote: remote,
		})
	}
	return out
}

// key identifies a mapping; it is the map key used throughout the package.
func key(m netprov.Mapping) string {
	proto := m.Proto
	if proto == "" {
		proto = "tcp"
	}
	return proto + " " + m.Local + " " + m.Remote
}
