package resolver

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// A trimmed "scutil --dns" from a Mac on a VPN: the search list is spread
// over several resolvers, most specific first, with repeats.
const scutilOutput = `DNS configuration

resolver #1
  search domain[0] : corp.example
  search domain[1] : eng.corp.example
  nameserver[0] : 10.0.0.1
  flags    : Request A records

resolver #2
  domain   : local
  options  : mdns

resolver #3
  search domain[0] : corp.example
  nameserver[0] : 10.0.0.2

DNS configuration (for scoped queries)

resolver #1
  search domain[0] : home.example
`

func TestParseSearchDomains(t *testing.T) {
	got := ParseSearchDomains(scutilOutput)
	want := []string{"corp.example", "eng.corp.example", "home.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := ParseSearchDomains(""); got != nil {
		t.Errorf("no output must give no domains, got %v", got)
	}
}

// macOS limits the search list; so must we, or the guest's resolv.conf is
// silently ignored by libc.
func TestSearchDomainsAreCapped(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 12; i++ {
		b.WriteString("  search domain[0] : d")
		b.WriteString(string(rune('a' + i)))
		b.WriteString(".example\n")
	}
	if got := ParseSearchDomains(b.String()); len(got) != maxSearchDomains {
		t.Errorf("got %d domains, want %d", len(got), maxSearchDomains)
	}
	long := "  search domain[0] : " + strings.Repeat("x", 300) + ".example\n"
	if got := ParseSearchDomains(long); len(got) != 0 {
		t.Errorf("an over-long domain was accepted: %v", got)
	}
}

// Host configuration is not trusted blindly: it ends up in a file in the
// guest.
func TestImplausibleSearchDomainsRejected(t *testing.T) {
	out := "  search domain[0] : ok.example\n  search domain[1] : bad domain\n  search domain[2] : also\tbad\n"
	got := ParseSearchDomains(out)
	if !reflect.DeepEqual(got, []string{"ok.example"}) {
		t.Errorf("got %v, want [ok.example]", got)
	}
}

func TestHostNames(t *testing.T) {
	old := runCommand
	defer func() { runCommand = old }()
	runCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "scutil" && len(args) == 2 && args[0] == "--get" && args[1] == "LocalHostName" {
			return "Darwin\n", nil
		}
		return "", nil
	}
	got := HostNames(context.Background())
	joined := strings.Join(got, " ")
	if !strings.Contains(strings.ToLower(joined), "darwin.local") {
		t.Errorf("HostNames = %v, want the .local name among them", got)
	}
	// No duplicates, whatever the kernel hostname is.
	seen := map[string]bool{}
	for _, n := range got {
		if seen[strings.ToLower(n)] {
			t.Errorf("duplicate name %q in %v", n, got)
		}
		seen[strings.ToLower(n)] = true
	}
}

func TestHostResolverIsAvailableInThisBuild(t *testing.T) {
	// A jm that cannot reach the host resolver silently loses VPN,
	// /etc/hosts and .local names (ADR 0008); the tests must notice.
	// On darwin that is not a matter of build luck — the libSystem path is
	// compiled with or without cgo — so a false here is a build-tag
	// regression and must fail, not skip.
	if HostResolver {
		return
	}
	if runtime.GOOS == "darwin" {
		t.Fatalf("HostResolver is false on darwin: the build tags no longer track "+
			"the standard library, which compiles the libSystem resolver on darwin "+
			"regardless of cgo (netgo = %v)", netgoBuild)
	}
	t.Skip("built without cgo off darwin; jm doctor reports this")
}
