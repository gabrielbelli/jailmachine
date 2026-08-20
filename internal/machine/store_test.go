package machine

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	s := NewStore(t.TempDir())
	m := Defaults()
	m.Name = "alpha"
	m.Backend = "qemu"
	m.Created = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	m.BackendOpts["backend.qemu.accel"] = "hvf"
	if err := s.Save(&m); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.Load("alpha")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Version != Version || got.Name != "alpha" || got.CPUs != 4 || got.MemoryMiB != 4096 ||
		got.DiskGiB != 64 || got.SSHPort != 2222 || got.SSHUser != "root" || got.Backend != "qemu" ||
		!got.Created.Equal(m.Created) || got.BackendOpts["backend.qemu.accel"] != "hvf" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.Dir != s.Dir("alpha") || m.Dir != s.Dir("alpha") {
		t.Fatalf("Dir not filled in: load=%q save=%q", got.Dir, m.Dir)
	}
	if data, _ := os.ReadFile(s.Path("alpha", RecordFile)); strings.Contains(string(data), s.Dir("alpha")) {
		t.Fatal("Dir must not be serialised into machine.json")
	}
	// no temp files left behind
	entries, _ := os.ReadDir(s.Dir("alpha"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLoadMissing(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := s.Load("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Save(&Machine{Name: "../evil"}); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestList(t *testing.T) {
	s := NewStore(t.TempDir())
	if got, err := s.List(); err != nil || len(got) != 0 {
		t.Fatalf("empty list: %v %v", got, err)
	}
	for _, n := range []string{"bravo", "alpha"} {
		m := Defaults()
		m.Name = n
		if err := s.Save(&m); err != nil {
			t.Fatal(err)
		}
	}
	// a stray directory without a record is ignored
	os.MkdirAll(s.Dir("stray"), 0o700)
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "bravo" {
		t.Fatalf("unexpected list: %+v", got)
	}
}

func TestDelete(t *testing.T) {
	s := NewStore(t.TempDir())
	m := Defaults()
	m.Name = "gone"
	if err := s.Save(&m); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	if s.Exists("gone") {
		t.Fatal("still exists")
	}
	if _, err := os.Stat(s.Dir("gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory still present")
	}
	if err := s.Delete("gone"); err != nil {
		t.Fatalf("second delete must be a no-op: %v", err)
	}
}

// TestLockExclusive re-executes the test binary as a child process that
// tries to take the same lock, since flock is per-process.
func TestLockExclusive(t *testing.T) {
	if os.Getenv("JM_LOCK_CHILD") != "" {
		s := NewStore(os.Getenv("JM_LOCK_ROOT"))
		_, err := s.Lock("locked")
		if errors.Is(err, ErrLocked) {
			os.Exit(0)
		}
		os.Exit(1)
	}
	root := t.TempDir()
	s := NewStore(root)
	unlock, err := s.Lock("locked")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockExclusive$")
	cmd.Env = append(os.Environ(), "JM_LOCK_CHILD=1", "JM_LOCK_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child should see ErrLocked while parent holds lock: %v\n%s", err, out)
	}
	unlock()
	// after unlock the child succeeds in acquiring, so it exits 1
	cmd = exec.Command(os.Args[0], "-test.run=^TestLockExclusive$")
	cmd.Env = append(os.Environ(), "JM_LOCK_CHILD=1", "JM_LOCK_ROOT="+root)
	if err := cmd.Run(); err == nil {
		t.Fatal("child should acquire the lock after parent released it")
	}
	// and the parent can re-take it
	unlock2, err := s.Lock("locked")
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}
