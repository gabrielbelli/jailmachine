package forwarder

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// DefaultHostIP is the host address a mapping binds to when podman reports
// an empty host_ip (plain "-p 8080:80").
const DefaultHostIP = "127.0.0.1"

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
func Desired(psJSON []byte, guestIP string) ([]netprov.Mapping, error) {
	out, _, err := Plan(psJSON, guestIP)
	return out, err
}

// Plan is Desired plus the published ports that cannot be reached from
// the host: a host_ip that is neither a wildcard nor the guest's own
// address (typically "-p 127.0.0.1:8080:80") makes podman in the guest
// bind that address only, so the guest's loopback, not the host's, gets
// the port. Those come back as entries with Error set and no Remote, so
// "jm ports" can say why they are unreachable instead of listing them as
// ok.
func Plan(psJSON []byte, guestIP string) ([]netprov.Mapping, []Entry, error) {
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
			for _, mp := range expand(p, guestIP) {
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

// publishable reports whether a host_ip podman bound in the guest is
// reachable from outside the guest: empty, a wildcard, or the guest's own
// address.
func publishable(hostIP, guestIP string) bool {
	switch hostIP {
	case "", "0.0.0.0", "::", guestIP:
		return true
	}
	return false
}

func unpublishableReason(p psPort) string {
	return fmt.Sprintf("guest binds %s only; publish with -p %d:%d (or 0.0.0.0)",
		p.HostIP, p.HostPort, p.ContainerPort)
}

// expand turns one port mapping (with its range) into one Mapping per host
// port. Mappings whose host_ip is not publishable (see publishable) have an
// empty Remote.
func expand(p psPort, guestIP string) []netprov.Mapping {
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	hostIP := p.HostIP
	if hostIP == "" {
		hostIP = DefaultHostIP
	}
	remoteIP := guestIP
	if !publishable(p.HostIP, guestIP) {
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
			Local:  net.JoinHostPort(hostIP, strconv.Itoa(port)),
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
