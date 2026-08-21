package forwarder

import (
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// Container ids from testdata/ps-shapes.json, by the shape each container
// was started with.
const (
	cPlain   = "708fbceaf60a49fc594fe71fb962043bb97000d6db065a2e9de6d020fc93a614"
	cWild    = "b77257e45a77ba2d4449b3e88d823b2f072d94586145f6a7925793594b7c31e2"
	cLoop    = "04df36ec8eadb4b5e529887c41a6741778d8c33f9398f34332f769cf4c65383e"
	cRange   = "2eb2007dc9d3be823f9d2ef411e473afda80f24f6243acb41afdafdc78f1b241"
	cV6Loop  = "fc7c3b760eb6a84b0b056152689a973b3e94dbd2b5d489388e3136f0e270687d"
	cUDP     = "f1631f76323f8c92bc563f8b0c4cfc735789a2ffb3ca3cc4dc9466cf2c7ce8d2"
	cLoRange = "3c0a9d1f5b8e4a27a1c6d0e93f7b21548ac6e0d4b9f3172e5c8a06d4e1f2b933"
	cGuest   = "9f14b2c7d3e58a604b1f7c2093ad85e6f0c4b7128d3a95e6c07b1f4a2d63e850"
	cUDPLoop = "5d72e91a08c34b6f9e2d17a5c840b3f6721e9d0a4c58b3e27f16094ad8c5b2e1"
)

// sorted is the ascending copy of ids, which is the order ContainerIDs
// returns them in.
func sorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func shapes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/ps-shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func inspected(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile("testdata/inspect-shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	ips, err := ParseInspect(data)
	if err != nil {
		t.Fatal(err)
	}
	return ips
}

// TestDesiredRealShapes runs the desired-state computation over
// testdata/ps-shapes.json, which is "podman ps --format json" from a real
// machine running one container per publish shape:
//
//	-p 8080:80                   plain, the docker default
//	-p 0.0.0.0:8081:80           the wildcard spelt out
//	-p 127.0.0.1:8082:80         a host address
//	-p 8083-8085:80-82           a range
//	-p 8086:8086/udp             udp
//	-p [::1]:8087:80             an IPv6 host address
//	-p 192.168.127.2:8096:80     the guest's own address
//	-p 127.0.0.1:8095:5000/udp   a host address and udp
//	-p 127.0.0.1:8110-8111:80-81 a host address and a range
//
// Every shape is published; the host side is what docker would bind. The
// three that name a host address the guest does not have are the ones the
// engine leaves unreachable inside the guest, so they come with a rule for
// jm's own pf anchor (see Rule) rather than an apology.
func TestDesiredRealShapes(t *testing.T) {
	pl, err := Compute(shapes(t), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []netprov.Mapping{
		// Nothing named a host address: the machine's publish address
		// decides, and the engine already redirects the guest's own
		// address, so no rule of ours is needed.
		{Proto: "tcp", Local: "0.0.0.0:8080", Remote: guest + ":8080"},
		{Proto: "tcp", Local: "0.0.0.0:8081", Remote: guest + ":8081"},
		{Proto: "tcp", Local: "0.0.0.0:8083", Remote: guest + ":8083"},
		{Proto: "tcp", Local: "0.0.0.0:8084", Remote: guest + ":8084"},
		{Proto: "tcp", Local: "0.0.0.0:8085", Remote: guest + ":8085"},
		{Proto: "tcp", Local: "0.0.0.0:8096", Remote: guest + ":8096"},
		// A host address in the flag confines the host side to it,
		// exactly as docker does, whatever the machine's default is.
		{Proto: "tcp", Local: "127.0.0.1:8082", Remote: guest + ":8082"},
		{Proto: "tcp", Local: "127.0.0.1:8110", Remote: guest + ":8110"},
		{Proto: "tcp", Local: "127.0.0.1:8111", Remote: guest + ":8111"},
		{Proto: "tcp", Local: "[::1]:8087", Remote: guest + ":8087"},
		{Proto: "udp", Local: "0.0.0.0:8086", Remote: guest + ":8086"},
		{Proto: "udp", Local: "127.0.0.1:8095", Remote: guest + ":8095"},
	}
	if !reflect.DeepEqual(pl.Mappings, want) {
		t.Errorf("Compute mappings =\n%v\nwant\n%v", pl.Mappings, want)
	}
	if len(pl.Unpublishable) != 0 {
		t.Errorf("Unpublishable = %+v, want none: every shape podman accepts can be published", pl.Unpublishable)
	}
	if len(pl.Pending) != 0 {
		t.Errorf("Pending = %v, want none before the guest side is resolved", pl.Pending)
	}

	// Only the containers whose ports need a redirect are inspected: the
	// common shapes must not cost a round trip.
	wantIDs := []string{cLoop, cLoRange, cUDPLoop, cWild, cV6Loop}
	if got := pl.ContainerIDs(); !reflect.DeepEqual(got, sorted(wantIDs)) {
		t.Errorf("ContainerIDs = %v, want %v", got, sorted(wantIDs))
	}

	pl.Resolve(inspected(t))
	wantRules := []Rule{
		{Proto: "tcp", GuestIP: guest, GuestPort: 8081, ContainerID: cWild, ContainerIP: "10.88.0.11", ContainerPort: 80},
		{Proto: "tcp", GuestIP: guest, GuestPort: 8082, ContainerID: cLoop, ContainerIP: "10.88.0.15", ContainerPort: 80},
		{Proto: "tcp", GuestIP: guest, GuestPort: 8087, ContainerID: cV6Loop, ContainerIP: "10.88.0.17", ContainerPort: 80},
		{Proto: "udp", GuestIP: guest, GuestPort: 8095, ContainerID: cUDPLoop, ContainerIP: "10.88.0.23", ContainerPort: 5000},
		{Proto: "tcp", GuestIP: guest, GuestPort: 8110, ContainerID: cLoRange, ContainerIP: "10.88.0.19", ContainerPort: 80},
		{Proto: "tcp", GuestIP: guest, GuestPort: 8111, ContainerID: cLoRange, ContainerIP: "10.88.0.19", ContainerPort: 81},
	}
	if len(pl.Rules) != len(wantRules) {
		t.Fatalf("rules =\n%v\nwant\n%v", pl.Rules, wantRules)
	}
	for i, r := range pl.Rules {
		w := wantRules[i]
		if r.Proto != w.Proto || r.GuestIP != w.GuestIP || r.GuestPort != w.GuestPort ||
			r.ContainerID != w.ContainerID || r.ContainerIP != w.ContainerIP || r.ContainerPort != w.ContainerPort {
			t.Errorf("rule %d = %+v, want %+v", i, r, w)
		}
	}
	// The anchor is loaded whole, so its text is the whole desired guest
	// side: a stale rule cannot survive a reconcile.
	wantText := "rdr pass inet proto tcp from any to 192.168.127.2 port = 8081 -> 10.88.0.11 port 80\n" +
		"rdr pass inet proto tcp from any to 192.168.127.2 port = 8082 -> 10.88.0.15 port 80\n" +
		"rdr pass inet proto tcp from any to 192.168.127.2 port = 8087 -> 10.88.0.17 port 80\n" +
		"rdr pass inet proto udp from any to 192.168.127.2 port = 8095 -> 10.88.0.23 port 5000\n" +
		"rdr pass inet proto tcp from any to 192.168.127.2 port = 8110 -> 10.88.0.19 port 80\n" +
		"rdr pass inet proto tcp from any to 192.168.127.2 port = 8111 -> 10.88.0.19 port 81\n"
	if got := AnchorText(pl.Rules); got != wantText {
		t.Errorf("AnchorText =\n%q\nwant\n%q", got, wantText)
	}
	if got := AnchorScript(""); !strings.Contains(got, "pfctl -a "+GuestAnchor+" -f -") {
		t.Errorf("AnchorScript = %q", got)
	}
}

// A publish that names no host address follows the machine's publish
// address; one that names a host address does not. That is docker's rule,
// and the only one under which "-p 127.0.0.1:8082:80" still means the
// loopback on a machine that publishes on the LAN by default, and still
// means the loopback on a machine that does not.
func TestPublishAddrIsOnlyTheDefault(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "192.168.0.18"} {
		pl, err := Compute(shapes(t), guest, addr)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, mp := range pl.Mappings {
			host, _, err := net.SplitHostPort(mp.Local)
			if err != nil {
				t.Fatalf("%v: %v", mp, err)
			}
			got[mp.Local] = host
		}
		for local, host := range got {
			switch {
			case strings.HasPrefix(local, "127.0.0.1:8082"), strings.HasPrefix(local, "127.0.0.1:811"),
				strings.HasPrefix(local, "127.0.0.1:8095"):
				if host != "127.0.0.1" {
					t.Errorf("--publish-addr %s moved an explicit -p 127.0.0.1 to %s", addr, host)
				}
			case strings.HasPrefix(local, "[::1]"):
				if host != "::1" {
					t.Errorf("--publish-addr %s moved an explicit -p [::1] to %s", addr, host)
				}
			case strings.Contains(local, ":8081"):
				// "-p 0.0.0.0:8081:80" spells out every interface.
				if host != DefaultHostIP {
					t.Errorf("--publish-addr %s narrowed an explicit -p 0.0.0.0 to %s", addr, host)
				}
			default:
				if host != addr {
					t.Errorf("%s binds %s, want the machine's publish address %s", local, host, addr)
				}
			}
		}
	}
}

// Two containers cannot both own the same guest port: the engine's own
// redirect for a plain publish takes precedence, and the second container's
// mapping says so rather than silently pointing at the first one's.
func TestGuestPortClash(t *testing.T) {
	const ps = `[
	 {"Id":"a1","Names":["plain"],"State":"running","Ports":[
	   {"host_ip":"","container_port":80,"host_port":8300,"range":1,"protocol":"tcp"}]},
	 {"Id":"b2","Names":["lo"],"State":"running","Ports":[
	   {"host_ip":"127.0.0.1","container_port":80,"host_port":8300,"range":1,"protocol":"tcp"}]},
	 {"Id":"c3","Names":["lo6"],"State":"running","Ports":[
	   {"host_ip":"::1","container_port":80,"host_port":8301,"range":1,"protocol":"tcp"}]},
	 {"Id":"d4","Names":["wild"],"State":"running","Ports":[
	   {"host_ip":"0.0.0.0","container_port":80,"host_port":8301,"range":1,"protocol":"tcp"}]}
	]`
	pl, err := Compute([]byte(ps), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	// Every host leg is still desired: the host binds are distinct and
	// docker would bind them too.
	if len(pl.Mappings) != 4 {
		t.Fatalf("mappings = %v", pl.Mappings)
	}
	// 8300 belongs to the plain publish; the loopback one cannot have a
	// rule, and is told which container holds the port.
	clash := pl.Pending["tcp 127.0.0.1:8300 "+guest+":8300"]
	if !strings.Contains(clash, "8300/tcp") || !strings.Contains(clash, "different host port") ||
		!strings.Contains(clash, "container a1") {
		t.Errorf("clash with the engine's own redirect = %q", clash)
	}
	// 8301 is claimed by whichever of the two host-bound publishes came
	// first; the other is told, not silently pointed at the wrong
	// container.
	if len(pl.Rules) != 1 || pl.Rules[0].GuestPort != 8301 || pl.Rules[0].ContainerID != "c3" {
		t.Fatalf("rules = %+v", pl.Rules)
	}
	if got := pl.Pending["tcp 0.0.0.0:8301 "+guest+":8301"]; !strings.Contains(got, "8301/tcp") ||
		!strings.Contains(got, "container c3") {
		t.Errorf("clash between two host-bound publishes = %q", got)
	}
}

// A container can clash with itself: "-p 8080:80 -p 127.0.0.1:8080:81"
// publishes one guest port twice, and the engine's own redirect for the
// plain half wins. Blaming "another container" would send the user hunting
// for a container that is not there.
func TestGuestPortClashWithItself(t *testing.T) {
	const ps = `[
	 {"Id":"a1b2c3d4e5f60718","Names":["self"],"State":"running","Ports":[
	   {"host_ip":"","container_port":80,"host_port":8080,"range":1,"protocol":"tcp"},
	   {"host_ip":"127.0.0.1","container_port":81,"host_port":8080,"range":1,"protocol":"tcp"}]}
	]`
	pl, err := Compute([]byte(ps), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Rules) != 0 {
		t.Fatalf("rules = %+v, want none: the engine already owns 8080 in the guest", pl.Rules)
	}
	got := pl.Pending["tcp 127.0.0.1:8080 "+guest+":8080"]
	if !strings.Contains(got, "this container's own publish") {
		t.Errorf("self-clash = %q, want it to say the container clashes with itself", got)
	}
	if strings.Contains(got, "another container") {
		t.Errorf("self-clash = %q, blames a container that does not exist", got)
	}
}

// A container whose address the engine has not assigned yet keeps its host
// leg and says why it is not working; the next reconcile picks it up. The
// alternative — dropping the mapping — would unpublish a port because one
// inspect was early.
func TestResolveWithoutAnAddress(t *testing.T) {
	pl, err := Compute(shapes(t), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	ips := inspected(t)
	delete(ips, cLoop)
	pl.Resolve(ips)
	if len(pl.Rules) != 5 {
		t.Errorf("rules = %d, want the five that resolved", len(pl.Rules))
	}
	if got := pl.Pending["tcp 127.0.0.1:8082 "+guest+":8082"]; !strings.Contains(got, "no address") {
		t.Errorf("pending = %q", got)
	}
	if AnchorText(pl.Rules) == "" {
		t.Error("the rules that did resolve must still be loaded")
	}

	// A guest-side step that fails wholesale blocks every mapping that
	// depends on it, and none of the others.
	pl2, err := Compute(shapes(t), guest, "")
	if err != nil {
		t.Fatal(err)
	}
	pl2.Block("pfctl: permission denied")
	if len(pl2.Pending) != 6 || len(pl2.Rules) != 0 {
		t.Fatalf("Block left %d pending, %d rules", len(pl2.Pending), len(pl2.Rules))
	}
	if _, blocked := pl2.Pending["tcp 0.0.0.0:8080 "+guest+":8080"]; blocked {
		t.Error("a plain publish was blocked by a guest-side failure it does not depend on")
	}
}

func TestParseInspect(t *testing.T) {
	ips := inspected(t)
	if ips[cLoop] != "10.88.0.15" {
		t.Errorf("per-network address = %q", ips[cLoop])
	}
	if ips[cUDPLoop] != "10.88.0.23" {
		t.Errorf("top-level address = %q", ips[cUDPLoop])
	}
	// A container with no address at all is absent, not empty-valued.
	got, err := ParseInspect([]byte(`[{"Id":"x1","NetworkSettings":{"IPAddress":"","Networks":{}}}]`))
	if err != nil || len(got) != 0 {
		t.Errorf("ParseInspect = %v, %v", got, err)
	}
	if got, err := ParseInspect(nil); err != nil || len(got) != 0 {
		t.Errorf("ParseInspect(nil) = %v, %v", got, err)
	}
	if _, err := ParseInspect([]byte("{not json")); err == nil {
		t.Error("garbage accepted")
	}
}

// A publish address is validated where it is typed, so that a typo is a
// usage error rather than a per-mapping expose failure minutes later.
func TestParsePublishAddr(t *testing.T) {
	for _, in := range []string{"", "  "} {
		if got, err := ParsePublishAddr(in); err != nil || got != "" {
			t.Errorf("ParsePublishAddr(%q) = %q, %v", in, got, err)
		}
	}
	if got, err := ParsePublishAddr(" 127.0.0.1 "); err != nil || got != "127.0.0.1" {
		t.Errorf("ParsePublishAddr = %q, %v", got, err)
	}
	for _, in := range []string{"localhost", "127.0.0.1:8080", "0.0.0.0.0"} {
		if _, err := ParsePublishAddr(in); err == nil {
			t.Errorf("ParsePublishAddr(%q) accepted", in)
		}
	}
	if got := HostIP(""); got != DefaultHostIP {
		t.Errorf("HostIP(\"\") = %q, want %q", got, DefaultHostIP)
	}
}
