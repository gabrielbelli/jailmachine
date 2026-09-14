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
	if got.Version != Version || got.Name != "alpha" || got.CPUs != 4 || got.MemoryMiB != 2048 || got.ArcMiB != DefaultArcMiB || got.IdleSuspendMin != DefaultIdleSuspendMin ||
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

func TestLoadLegacyRecordIsImageTrusted(t *testing.T) {
	// Records written before image_trusted existed came from the
	// checksummed official source: absent key loads as true, and an
	// explicit false survives.
	s := NewStore(t.TempDir())
	dir := s.Dir("old")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path("old", RecordFile), []byte(`{"version":1,"name":"old","image":"official:15.1-RELEASE"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := s.Load("old")
	if err != nil || !m.ImageTrusted {
		t.Fatalf("legacy record: %+v, %v", m, err)
	}
	if err := os.WriteFile(s.Path("old", RecordFile), []byte(`{"version":1,"name":"old","image":"byo:/x.raw","image_trusted":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err = s.Load("old"); err != nil || m.ImageTrusted {
		t.Fatalf("explicit false: %+v, %v", m, err)
	}
}

func TestLoadArcMiB(t *testing.T) {
	// Records written before arc_mib existed get the default cap; an
	// explicit 0 (the guest's own default) survives a save and load.
	s := NewStore(t.TempDir())
	if err := os.MkdirAll(s.Dir("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path("old", RecordFile), []byte(`{"version":1,"name":"old","memory_mib":4096}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := s.Load("old")
	if err != nil || m.ArcMiB != DefaultArcMiB || m.MemoryMiB != 4096 {
		t.Fatalf("legacy record: %+v, %v", m, err)
	}
	m.ArcMiB = 0
	if err := s.Save(m); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(s.Path("old", RecordFile)); !strings.Contains(string(data), `"arc_mib": 0`) {
		t.Fatalf("explicit 0 not written:\n%s", data)
	}
	if m, err = s.Load("old"); err != nil || m.ArcMiB != 0 {
		t.Fatalf("explicit 0: %+v, %v", m, err)
	}
	// A legacy record too small for the default cap gets one it can take.
	if err := os.WriteFile(s.Path("old", RecordFile), []byte(`{"version":1,"name":"old","memory_mib":512}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err = s.Load("old"); err != nil || m.ArcMiB != 256 {
		t.Fatalf("legacy 512 MiB record: %+v, %v", m, err)
	}
}

func TestDefaultArcFor(t *testing.T) {
	for mem, want := range map[int]int{4096: 512, 2048: 512, 1024: 512, 1000: 500, 512: 256, 256: 128, 128: 64, 127: 0, 0: 0} {
		if got := DefaultArcFor(mem); got != want {
			t.Errorf("DefaultArcFor(%d) = %d, want %d", mem, got, want)
		}
	}
}

func TestLoadSeedsIdleSuspendMin(t *testing.T) {
	// A record written before idle_suspend_min existed (the maintainer's
	// 4096 MiB machine among them) gets the default idle time.
	if d := Defaults(); d.IdleSuspendMin != DefaultIdleSuspendMin || DefaultIdleSuspendMin != 30 {
		t.Fatalf("Defaults().IdleSuspendMin = %d, want 30", d.IdleSuspendMin)
	}
	s := NewStore(t.TempDir())
	if err := os.MkdirAll(s.Dir("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path("old", RecordFile), []byte(`{"version":1,"name":"old","memory_mib":4096,"arc_mib":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := s.Load("old")
	if err != nil || m.IdleSuspendMin != DefaultIdleSuspendMin || m.ArcMiB != 0 || m.Version != 1 {
		t.Fatalf("legacy record: %+v, %v", m, err)
	}
	// An explicit non-default value is kept as written.
	if err := os.WriteFile(s.Path("old", RecordFile), []byte(`{"version":1,"name":"old","idle_suspend_min":120}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if m, err = s.Load("old"); err != nil || m.IdleSuspendMin != 120 {
		t.Fatalf("explicit 120: %+v, %v", m, err)
	}
}

func TestExplicitZeroIdleSuspendSurvives(t *testing.T) {
	s := NewStore(t.TempDir())
	m := Defaults()
	m.Name = "never"
	m.IdleSuspendMin = 0
	if err := s.Save(&m); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(s.Path("never", RecordFile)); !strings.Contains(string(data), `"idle_suspend_min": 0`) {
		t.Fatalf("explicit 0 not written:\n%s", data)
	}
	got, err := s.Load("never")
	if err != nil || got.IdleSuspendMin != 0 {
		t.Fatalf("explicit 0: %+v, %v", got, err)
	}
}
