package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

func TestParseChecksums(t *testing.T) {
	in := `# comment
SHA256 (FreeBSD-15.1-RELEASE-arm64-aarch64-BASIC-CLOUDINIT-zfs.raw.xz) = ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789
SHA256 (other.img) = 0000000000000000000000000000000000000000000000000000000000000000
garbage line
`
	sums, err := ParseChecksums(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(sums), sums)
	}
	got := sums["FreeBSD-15.1-RELEASE-arm64-aarch64-BASIC-CLOUDINIT-zfs.raw.xz"]
	if got != strings.ToLower("ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789") {
		t.Fatalf("unexpected digest %q", got)
	}
}

func TestGrow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(p, bytes.Repeat([]byte{1}, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Grow(p, 4096); err != nil {
		t.Fatal(err)
	}
	if size(t, p) != 4096 {
		t.Fatalf("grow: size %d", size(t, p))
	}
	if err := Grow(p, 10); err != nil {
		t.Fatal(err)
	}
	if size(t, p) != 4096 {
		t.Fatalf("shrunk to %d", size(t, p))
	}
	if err := Grow(filepath.Join(t.TempDir(), "missing"), 10); err == nil {
		t.Fatal("expected error for missing file")
	}
}

// fixture is a fake release directory: a payload, its xz archive and a
// CHECKSUM.SHA256 file, served by httptest with Range support.
type fixture struct {
	t        testing.TB
	payload  []byte
	archive  []byte
	sum      string
	src      *Official
	srv      *httptest.Server
	requests atomic.Int32
	ranges   []string
	// badSum makes CHECKSUM.SHA256 publish a wrong digest.
	badSum bool
}

func newFixture(t testing.TB, payload []byte) *fixture {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(buf.Bytes())
	f := &fixture{t: t, payload: payload, archive: buf.Bytes(), sum: hex.EncodeToString(h[:])}
	f.src = &Official{Release: "15.1-RELEASE", NoXZBinary: true}
	mux := http.NewServeMux()
	prefix := "/15.1-RELEASE/aarch64/Latest/"
	mux.HandleFunc(prefix+"CHECKSUM.SHA256", func(rw http.ResponseWriter, _ *http.Request) {
		sum := f.sum
		if f.badSum {
			sum = strings.Repeat("0", 64)
		}
		io.WriteString(rw, "SHA256 ("+f.src.FileName()+") = "+sum+"\n")
	})
	mux.HandleFunc(prefix+f.src.FileName(), func(rw http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		http.ServeContent(rw, r, "img.xz", time0, bytes.NewReader(f.archive))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.src.BaseURL = f.srv.URL
	f.src.Client = f.srv.Client()
	return f
}

func TestOfficialFetch(t *testing.T) {
	payload := bytes.Repeat([]byte("jailmachine "), 4096)
	f := newFixture(t, payload)
	dest := filepath.Join(t.TempDir(), "disk.raw")
	f.src.DiskGiB = 0
	var log bytes.Buffer
	if err := f.src.Fetch(context.Background(), dest, &log); err != nil {
		t.Fatalf("fetch: %v\n%s", err, log.String())
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("decompressed payload differs")
	}
	for _, leftover := range []string{dest + ".xz", dest + ".xz.part", dest + ".tmp"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", leftover)
		}
	}
	if f.src.URL() != f.srv.URL+"/15.1-RELEASE/aarch64/Latest/"+f.src.FileName() {
		t.Errorf("unexpected URL %s", f.src.URL())
	}
}

func TestOfficialFetchGrows(t *testing.T) {
	f := newFixture(t, []byte("tiny"))
	dest := filepath.Join(t.TempDir(), "disk.raw")
	f.src.DiskGiB = 1
	if err := f.src.Fetch(context.Background(), dest, nil); err != nil {
		t.Fatal(err)
	}
	if size(t, dest) != GiB {
		t.Fatalf("size %d, want %d", size(t, dest), GiB)
	}
}

func TestOfficialFetchResumes(t *testing.T) {
	f := newFixture(t, bytes.Repeat([]byte("resume me "), 2048))
	dest := filepath.Join(t.TempDir(), "disk.raw")
	// Simulate an interrupted download: first half already on disk.
	half := len(f.archive) / 2
	if err := os.WriteFile(dest+".xz.part", f.archive[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.src.Fetch(context.Background(), dest, nil); err != nil {
		t.Fatal(err)
	}
	if n := f.requests.Load(); n != 1 {
		t.Fatalf("want 1 image request, got %d", n)
	}
	if want := "bytes=" + itoa(half) + "-"; f.ranges[0] != want {
		t.Fatalf("Range header %q, want %q", f.ranges[0], want)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, f.payload) {
		t.Fatal("resumed payload differs")
	}
}

func TestOfficialFetchCorruptPartialRestarts(t *testing.T) {
	// A corrupt .part whose resumed download hashes wrong must be removed
	// and reported as ErrChecksum; a second Fetch then starts clean.
	f := newFixture(t, bytes.Repeat([]byte("x"), 8192))
	dest := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(dest+".xz.part", bytes.Repeat([]byte("!"), 100), 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.src.Fetch(context.Background(), dest, nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if _, err := os.Stat(dest + ".xz.part"); !os.IsNotExist(err) {
		t.Fatal("corrupt .part should have been removed")
	}
	if err := f.src.Fetch(context.Background(), dest, nil); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if f.ranges[1] != "" {
		t.Fatalf("second fetch should start from zero, sent Range %q", f.ranges[1])
	}
}

func TestOfficialFetchChecksumMismatch(t *testing.T) {
	f := newFixture(t, []byte("payload"))
	f.badSum = true
	dest := filepath.Join(t.TempDir(), "disk.raw")
	err := f.src.Fetch(context.Background(), dest, nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("dest must not exist after a checksum failure")
	}
}

func TestOfficialFetchMissingChecksumEntry(t *testing.T) {
	f := newFixture(t, []byte("payload"))
	f.src.Release = "15.1-RELEASE"
	// Point at a release dir the server does not know for the checksum
	// file: 404 must be an error, not a silent pass.
	f.src.BaseURL = f.srv.URL + "/nope"
	if err := f.src.Fetch(context.Background(), filepath.Join(t.TempDir(), "d"), nil); err == nil {
		t.Fatal("expected error for missing CHECKSUM.SHA256")
	}
}

func TestOfficialFetchWithXZBinary(t *testing.T) {
	bin := lookXZ()
	if bin == "" {
		t.Skip("xz not on PATH")
	}
	f := newFixture(t, bytes.Repeat([]byte("binary path "), 1024))
	f.src.NoXZBinary = false
	f.src.XZBinary = bin
	dest := filepath.Join(t.TempDir(), "disk.raw")
	if err := f.src.Fetch(context.Background(), dest, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, f.payload) {
		t.Fatal("payload differs via xz binary")
	}
}

func TestDefaults(t *testing.T) {
	o := &Official{}
	if o.Name() != "official" {
		t.Fatal(o.Name())
	}
	want := DefaultBaseURL + "/" + DefaultRelease + "/aarch64/Latest/FreeBSD-" + DefaultRelease + "-arm64-aarch64-BASIC-CLOUDINIT-zfs.raw.xz"
	if o.URL() != want {
		t.Fatalf("URL %s\nwant %s", o.URL(), want)
	}
}

var _ Source = (*Official)(nil)

func size(t testing.TB, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func itoa(n int) string {
	var b []byte
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// time0 is a fixed modtime so http.ServeContent emits Last-Modified and
// honours Range requests deterministically.
var time0 = mustTime()

func mustTime() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
