package forwarder

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gabrielbelli/jailmachine/internal/netprov"
)

// TestDesiredRealShapes runs the desired-state computation over
// testdata/ps-shapes.json, which is verbatim "podman ps --format json" from
// a real machine running one container per publish shape:
//
//	-p 8080:80            plain, the docker default
//	-p 0.0.0.0:8081:80    the wildcard spelt out
//	-p 127.0.0.1:8082:80  a host address
//	-p 8083-8085:80-82    a range
//	-p [::1]:8087:80      an IPv6 host address
//	-p 8086:8086/udp      udp
//
// The reachability of each shape was measured on that machine: the plain
// and range publishes answer on the host, the three that name a host
// address do not, because of what the engine does inside the guest (see
// publishable).
func TestDesiredRealShapes(t *testing.T) {
	data, err := os.ReadFile("testdata/ps-shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	desired, skipped, err := Plan(data, guest, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []netprov.Mapping{
		{Proto: "tcp", Local: "0.0.0.0:8080", Remote: guest + ":8080"},
		{Proto: "tcp", Local: "0.0.0.0:8083", Remote: guest + ":8083"},
		{Proto: "tcp", Local: "0.0.0.0:8084", Remote: guest + ":8084"},
		{Proto: "tcp", Local: "0.0.0.0:8085", Remote: guest + ":8085"},
		{Proto: "udp", Local: "0.0.0.0:8086", Remote: guest + ":8086"},
	}
	if !reflect.DeepEqual(desired, want) {
		t.Errorf("Plan desired =\n%v\nwant\n%v", desired, want)
	}
	wantSkipped := map[string]string{
		"0.0.0.0:8081":   "never matches",
		"127.0.0.1:8082": "guest's own loopback",
		"[::1]:8087":     "guest's own loopback",
	}
	if len(skipped) != len(wantSkipped) {
		t.Fatalf("Plan skipped = %+v", skipped)
	}
	for _, e := range skipped {
		want, ok := wantSkipped[e.Local]
		if !ok {
			t.Errorf("unexpected skipped entry %+v", e)
			continue
		}
		if e.Remote != "" || !strings.Contains(e.Error, want) {
			t.Errorf("skipped %s: %+v, want a reason mentioning %q", e.Local, e, want)
		}
	}
}

// TestPublishAddrOverride: a host that does not want containers on the LAN
// binds published ports somewhere else, without the guest knowing. The
// address is a parameter, not ambient process state: the forwarder runs
// detached, so a value read from its own environment would be invisible to
// "jm inspect" and would not survive the next plain "jm start".
func TestPublishAddrOverride(t *testing.T) {
	data, err := os.ReadFile("testdata/ps-shapes.json")
	if err != nil {
		t.Fatal(err)
	}
	desired, _, err := Plan(data, guest, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) == 0 {
		t.Fatal("no mappings")
	}
	for _, mp := range desired {
		if got := hostOf(mp.Local); got != "127.0.0.1" {
			t.Errorf("mapping %v binds %q, want 127.0.0.1", mp, got)
		}
	}
}

// hostOf is the address half of a "host:port" local address.
func hostOf(local string) string {
	if i := strings.LastIndex(local, ":"); i >= 0 {
		return local[:i]
	}
	return local
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
