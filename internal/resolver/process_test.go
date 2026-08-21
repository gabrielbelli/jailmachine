package resolver

import (
	"path/filepath"
	"testing"
)

func TestProcessIdentity(t *testing.T) {
	dir := t.TempDir()
	p := Process{Dir: dir, Name: "dev", Root: "/state root"}
	if got := p.Args(); len(got) != 4 || got[0] != "--state-root" || got[3] != "dev" {
		t.Fatalf("Args = %v", got)
	}
	// A pid is only ours when both the state root and the machine name are
	// in its argv: pids are recycled and pid files outlive reboots.
	ours := "/usr/local/bin/jm --state-root /state root _resolver dev"
	if !IsOurs(ours, p) {
		t.Errorf("own argv not recognised: %q", ours)
	}
	for _, argv := range []string{
		"",
		"/usr/local/bin/jm --state-root /other _resolver dev",
		"/usr/local/bin/jm --state-root /state root _resolver other",
		"/usr/local/bin/jm --state-root /state root _forwarder dev",
	} {
		if IsOurs(argv, p) {
			t.Errorf("argv wrongly recognised as ours: %q", argv)
		}
	}
	if p.LogPath() != filepath.Join(dir, LogFile) || p.AddrPath() != filepath.Join(dir, AddrFile) {
		t.Errorf("paths = %q, %q", p.LogPath(), p.AddrPath())
	}
}

func TestPublishedAddress(t *testing.T) {
	p := Process{Dir: t.TempDir(), Name: "dev", Root: "/state"}
	if p.Addr() != "" || p.Port() != 0 {
		t.Errorf("a machine with no resolver has no address: %q, %d", p.Addr(), p.Port())
	}
	if err := p.PublishAddr("127.0.0.1:53535"); err != nil {
		t.Fatal(err)
	}
	if p.Addr() != "127.0.0.1:53535" || p.Port() != 53535 {
		t.Errorf("Addr = %q, Port = %d", p.Addr(), p.Port())
	}
	if err := p.PublishAddr("nonsense"); err != nil {
		t.Fatal(err)
	}
	if p.Port() != 0 {
		t.Errorf("a malformed address must have no port, got %d", p.Port())
	}
	// The port outlives the live address, so a restarted resolver comes
	// back where the guest already expects it.
	if p.LastPort() != 53535 {
		t.Errorf("LastPort = %d, want the remembered 53535", p.LastPort())
	}
}

// A resolver that was never started is stopped, and stopping it is a no-op.
func TestAliveAndStopWithoutAProcess(t *testing.T) {
	p := Process{Dir: t.TempDir(), Name: "dev", Root: "/state"}
	if _, ok := p.Alive(); ok {
		t.Error("Alive on a machine with no pid file")
	}
	if err := p.Stop(t.Context()); err != nil {
		t.Errorf("Stop with no pid file: %v", err)
	}
}
