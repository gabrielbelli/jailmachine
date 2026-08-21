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

// DefaultHostIP is the host address a published port binds to when the
// command line names none: every interface, as "docker run -p 8080:80" does
// on Linux. The provider's listener is dual-stack, so 127.0.0.1, ::1,
// "localhost" and the machine's LAN address all reach the container.
const DefaultHostIP = "0.0.0.0"

// PublishAddrEnv overrides that address for people who do not want
// containers on the LAN: JM_PUBLISH_ADDR=127.0.0.1 keeps published ports on
// the host's loopback. It is a host-side default; an address written into
// the publish flag itself ("-p 127.0.0.1:8080:80") wins over it, as it does
// under docker. "jm start" reads it and writes it onto the machine record,
// so that the address a detached forwarder is really binding is the one
// "jm inspect" and "jm ports" show, rather than ambient state of the shell
// that happened to boot the machine.
const PublishAddrEnv = "JM_PUBLISH_ADDR"

// GuestAnchor is the pf anchor jm loads its own redirect rules into inside
// the guest. It is a sub-anchor of the "rdr/*" anchor the guest image
// already declares, so the rules need no change to the guest's pf.conf.
const GuestAnchor = "rdr/jm"

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
	ID     string   `json:"Id"`
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

// Rule is one redirect jm installs inside the guest, in its own pf anchor.
//
// The engine in the guest treats the address in "-p <addr>:8080:80" as a
// guest-side bind address: it redirects that address only, and for the
// literal wildcard it writes a rule to 0.0.0.0 that no packet ever matches.
// Under docker the same address is a *host*-side bind address and the VM
// never sees it. jm reproduces docker's split by binding <addr> on the host
// (the provider's job) and making the port reachable at the guest's own
// address itself (this rule), which is where the provider delivers.
type Rule struct {
	Proto string
	// GuestIP:GuestPort is what the host leg delivers to.
	GuestIP   string
	GuestPort int
	// ContainerID identifies the container; ContainerIP is its address on
	// the guest's container network, which only "podman inspect" knows and
	// which changes every time the container restarts, so it is resolved
	// on every reconcile rather than remembered.
	ContainerID   string
	ContainerIP   string
	ContainerPort int
	// keys are the mapping keys that depend on this rule, so that a
	// failure to install it is reported on those mappings.
	keys []string
}

// String renders the rule as the line of pf.conf it becomes.
func (r Rule) String() string {
	return fmt.Sprintf("rdr pass inet proto %s from any to %s port = %d -> %s port %d",
		r.Proto, r.GuestIP, r.GuestPort, r.ContainerIP, r.ContainerPort)
}

// Plan is the whole desired publishing state derived from one container
// listing: what to bind on the host, what to redirect in the guest, and
// what cannot be published at all.
type Plan struct {
	// Mappings are the host legs to expose, sorted and deduplicated.
	Mappings []netprov.Mapping
	// Rules are the guest-side redirects some of those mappings need; the
	// common "-p 8080:80" needs none, because the engine in the guest
	// already redirects the guest's own address to the container.
	Rules []Rule
	// Unpublishable are published ports jm cannot reach at all, with the
	// reason "jm ports" prints. Nothing podman accepts lands here today;
	// it stays for shapes a future engine might invent.
	Unpublishable []Entry
	// Pending marks mappings whose host leg is desired but whose guest-side
	// redirect is not in place (a container with no address yet, a pfctl
	// that failed). Keyed like the owned set; the value is the reason. The
	// host leg is still bound — the mapping is live but not yet working,
	// exactly as it is between "podman run" and the next reconcile.
	Pending map[string]string
}

// Desired turns "podman ps --format json" output into the sorted,
// deduplicated set of host mappings. It is Compute without the guest-side
// half, for callers that only want the host legs.
func Desired(psJSON []byte, guestIP, hostIP string) ([]netprov.Mapping, error) {
	pl, err := Compute(psJSON, guestIP, hostIP)
	return pl.Mappings, err
}

// Compute derives the desired state from "podman ps --format json".
// Containers that are not running (ps -a output, exited entries) contribute
// nothing.
//
// hostIP is the machine's publish address, the default bind for a publish
// that names no host address. A publish that does name one binds that
// address instead, whatever the machine's default: under docker
// "--publish-addr" is a preference and "-p 127.0.0.1:8080:80" is an
// instruction.
func Compute(psJSON []byte, guestIP, hostIP string) (Plan, error) {
	hostIP = HostIP(hostIP)
	pl := Plan{Pending: map[string]string{}}
	var cs []psContainer
	if len(psJSON) > 0 {
		if err := json.Unmarshal(psJSON, &cs); err != nil {
			return pl, fmt.Errorf("parsing podman ps output: %w", err)
		}
	}
	var ports []published
	for _, c := range cs {
		if c.Exited || (c.State != "" && c.State != "running" && c.State != "paused") {
			continue
		}
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			ports = append(ports, expand(c, p, guestIP, hostIP)...)
		}
	}

	// The engine's own redirects come first: a port it already made
	// reachable at the guest's address must not have a jm rule written over
	// it, and a second container publishing the same guest port cannot have
	// one at all.
	engineOwns := map[string]string{}
	for _, p := range ports {
		if _, taken := engineOwns[p.guestKey()]; !taken && !p.needsRule {
			engineOwns[p.guestKey()] = p.containerID
		}
	}

	seen := map[string]bool{}
	ruleAt := map[string]int{}
	for _, p := range ports {
		mp := p.mapping(guestIP)
		k := key(mp)
		if seen[k] {
			continue
		}
		seen[k] = true
		pl.Mappings = append(pl.Mappings, mp)
		if !p.needsRule {
			continue
		}
		gk := p.guestKey()
		if owner, taken := engineOwns[gk]; taken {
			pl.Pending[k] = portClash(p.hostPort, p.proto, owner, p.containerID)
			continue
		}
		if i, dup := ruleAt[gk]; dup {
			if pl.Rules[i].ContainerID != p.containerID {
				pl.Pending[k] = portClash(p.hostPort, p.proto, pl.Rules[i].ContainerID, p.containerID)
				continue
			}
			pl.Rules[i].keys = append(pl.Rules[i].keys, k)
			continue
		}
		ruleAt[gk] = len(pl.Rules)
		pl.Rules = append(pl.Rules, Rule{
			Proto: p.proto, GuestIP: guestIP, GuestPort: p.hostPort,
			ContainerID: p.containerID, ContainerPort: p.containerPort,
			keys: []string{k},
		})
	}
	sort.Slice(pl.Mappings, func(i, j int) bool { return key(pl.Mappings[i]) < key(pl.Mappings[j]) })
	sort.Slice(pl.Rules, func(i, j int) bool {
		if pl.Rules[i].GuestPort != pl.Rules[j].GuestPort {
			return pl.Rules[i].GuestPort < pl.Rules[j].GuestPort
		}
		return pl.Rules[i].Proto < pl.Rules[j].Proto
	})
	sort.Slice(pl.Unpublishable, func(i, j int) bool {
		return key(pl.Unpublishable[i].Mapping()) < key(pl.Unpublishable[j].Mapping())
	})
	return pl, nil
}

// portClash explains why a guest port cannot take jm's redirect: something
// already publishes it there. The owner is often the container itself
// ("-p 8080:80 -p 127.0.0.1:8080:81" publishes one guest port twice), and
// blaming "another container" would send the user hunting for a container
// that does not exist, so the message names the id either way.
func portClash(port int, proto, owner, self string) string {
	who := "container " + shortID(owner) + "'s publish"
	if owner == self {
		who = "this container's own publish of a different port"
	}
	return fmt.Sprintf("port %d/%s on the guest already carries %s; use a different host port", port, proto, who)
}

// shortID abbreviates a container id the way the engine's own output does.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// published is one port of one running container after its range has been
// unrolled: everything the two legs need.
type published struct {
	proto         string
	containerID   string
	hostPort      int
	containerPort int
	// bind is the host address this port binds, already resolved against
	// the machine's publish address.
	bind string
	// needsRule is whether the engine left the port unreachable at the
	// guest's own address, so jm has to redirect it itself.
	needsRule bool
}

func (p published) mapping(guestIP string) netprov.Mapping {
	return netprov.Mapping{
		Proto:  p.proto,
		Local:  net.JoinHostPort(p.bind, strconv.Itoa(p.hostPort)),
		Remote: net.JoinHostPort(guestIP, strconv.Itoa(p.hostPort)),
	}
}

// guestKey identifies the guest-side port a redirect would occupy.
func (p published) guestKey() string { return p.proto + "/" + strconv.Itoa(p.hostPort) }

// bindFor maps a published port's host_ip onto the host address the mapping
// binds and whether jm must install the guest-side redirect itself.
//
//	""                 the engine redirects every address the guest has,
//	                   including its own, so the host leg is enough; the
//	                   machine's publish address decides the host side.
//	the guest's own IP the engine redirects exactly that address: reachable
//	                   as it stands. Not a host address, so the host side
//	                   again follows the machine's publish address.
//	0.0.0.0 or ::      docker publishes on every host interface; the engine
//	                   wrote a redirect to the wildcard itself, which
//	                   matches nothing, so jm needs its own.
//	anything else      docker binds that host address and only that one;
//	                   the engine bound it inside the guest, where it is of
//	                   no use to anybody, so jm needs its own redirect.
func bindFor(portHostIP, guestIP, hostIP string) (bind string, needsRule bool) {
	switch portHostIP {
	case "":
		return hostIP, false
	case guestIP:
		return hostIP, false
	case "0.0.0.0", "::":
		return DefaultHostIP, true
	}
	return portHostIP, true
}

// expand unrolls one port mapping's range into one published port each.
func expand(c psContainer, p psPort, guestIP, hostIP string) []published {
	proto := p.Protocol
	if proto == "" {
		proto = "tcp"
	}
	bind, needsRule := bindFor(p.HostIP, guestIP, hostIP)
	n := int(p.Range)
	if n < 1 {
		n = 1
	}
	out := make([]published, 0, n)
	for i := 0; i < n; i++ {
		hp := int(p.HostPort) + i
		if hp > 65535 {
			break
		}
		out = append(out, published{
			proto: proto, containerID: c.ID, hostPort: hp,
			containerPort: int(p.ContainerPort) + i, bind: bind, needsRule: needsRule,
		})
	}
	return out
}

// ContainerIDs lists the containers whose address the rules need, sorted
// and deduplicated, for one batched "podman inspect".
func (pl Plan) ContainerIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range pl.Rules {
		if r.ContainerID == "" || seen[r.ContainerID] {
			continue
		}
		seen[r.ContainerID] = true
		out = append(out, r.ContainerID)
	}
	sort.Strings(out)
	return out
}

// Resolve fills the rules in with the container addresses from ParseInspect.
// A container with no address yet (still starting, or on no network) blocks
// its mappings rather than losing them: the next reconcile retries.
func (pl *Plan) Resolve(ips map[string]string) {
	kept := pl.Rules[:0]
	for _, r := range pl.Rules {
		ip := ips[r.ContainerID]
		if ip == "" {
			pl.block(r, "the container has no address on the guest's container network yet")
			continue
		}
		r.ContainerIP = ip
		kept = append(kept, r)
	}
	pl.Rules = kept
}

// Block marks every mapping that depends on a guest-side redirect as not
// working yet, with why: what the whole guest-side step failed with.
func (pl *Plan) Block(why string) {
	for _, r := range pl.Rules {
		pl.block(r, why)
	}
	pl.Rules = nil
}

func (pl *Plan) block(r Rule, why string) {
	if pl.Pending == nil {
		pl.Pending = map[string]string{}
	}
	for _, k := range r.keys {
		pl.Pending[k] = why
	}
}

// AnchorText renders the rules as the complete content of jm's pf anchor in
// the guest. It is loaded whole on every change, so the anchor is a pure
// function of the desired state: nothing accumulates, and a forwarder that
// starts on a guest with leftovers rewrites them.
func AnchorText(rules []Rule) string {
	var b strings.Builder
	for _, r := range rules {
		if r.ContainerIP == "" {
			continue
		}
		b.WriteString(r.String())
		b.WriteByte('\n')
	}
	return b.String()
}

// AnchorScript is the shell command that loads text as the whole content of
// jm's anchor inside the guest. Empty text flushes it.
func AnchorScript(text string) string {
	return "pfctl -a " + GuestAnchor + " -f - <<'JM_RULES_EOF'\n" + text + "JM_RULES_EOF\n"
}

// inspectContainer is the subset of "podman inspect --type container" we
// need: the address the container answers on inside the guest. podman fills
// in either the top-level field or the per-network one depending on the
// network backend, so both are read.
type inspectContainer struct {
	ID              string `json:"Id"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
		Networks  map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ParseInspect reads container id -> guest-network address out of
// "podman inspect --type container --format json" output.
func ParseInspect(data []byte) (map[string]string, error) {
	out := map[string]string{}
	if len(data) == 0 {
		return out, nil
	}
	var cs []inspectContainer
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parsing podman inspect output: %w", err)
	}
	for _, c := range cs {
		if c.ID == "" {
			continue
		}
		ip := c.NetworkSettings.IPAddress
		if ip == "" {
			// Deterministic pick: a container on several networks would
			// otherwise get a different rule on every reconcile.
			for _, name := range sortedKeys(c.NetworkSettings.Networks) {
				if a := c.NetworkSettings.Networks[name].IPAddress; a != "" {
					ip = a
					break
				}
			}
		}
		if ip != "" {
			out[c.ID] = ip
		}
	}
	return out, nil
}

// key identifies a mapping; it is the map key used throughout the package.
func key(m netprov.Mapping) string {
	proto := m.Proto
	if proto == "" {
		proto = "tcp"
	}
	return proto + " " + m.Local + " " + m.Remote
}
