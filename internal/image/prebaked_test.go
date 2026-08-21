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
	"syscall"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// sparsePayload is a disk-like payload: a little data, a large hole, a
// little data, and a trailing hole. The hole is 100 MiB because APFS
// silently fills holes smaller than a few tens of MiB (measured: 16 MiB is
// allocated, 64 MiB stays a hole), which is also why sparse-writing matters
// for a real 64 GiB image and not for small fixtures.
func sparsePayload() []byte {
	const mib = 1 << 20
	p := make([]byte, 200*mib)
	copy(p, bytes.Repeat([]byte("head"), 4096))
	copy(p[100*mib:], bytes.Repeat([]byte("tail"), 4096))
	return p
}

func zstdBytes(t testing.TB, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func xzBytes(t testing.TB, payload []byte) []byte {
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
	return buf.Bytes()
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// allocated returns the bytes actually allocated on disk for path.
func allocated(t testing.TB, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func TestParseSidecar(t *testing.T) {
	const h = "ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	cases := []struct {
		in, file string
		ok       bool
	}{
		{h + "  img.raw.zst\n", "img.raw.zst", true},
		{h + " *img.raw.zst\n", "img.raw.zst", true},
		{h + "\n", "img.raw.zst", true},
		{"# comment\n\n" + h + "  dir/img.raw.zst\n", "/x/img.raw.zst", true},
		{h + "  other.zst\n", "img.raw.zst", false},
		{"nothex  img.raw.zst\n", "img.raw.zst", false},
		{"", "img.raw.zst", false},
	}
	for _, c := range cases {
		got, err := ParseSidecar(strings.NewReader(c.in), c.file)
		if c.ok && (err != nil || got != strings.ToLower(h)) {
			t.Errorf("%q: got %q, %v", c.in, got, err)
		}
		if !c.ok && !errors.Is(err, ErrSidecar) {
			t.Errorf("%q: want ErrSidecar, got %v", c.in, err)
		}
	}
	if SidecarLine("ab", "/tmp/x.zst") != "ab  x.zst\n" {
		t.Error(SidecarLine("ab", "/tmp/x.zst"))
	}
}

func TestPrebakedNames(t *testing.T) {
	p := &Prebaked{}
	if p.Name() != "prebaked" {
		t.Fatal(p.Name())
	}
	want := DefaultReleaseBaseURL + "/guest-" + GuestVersion + "/jailmachine-guest-" + GuestVersion + "-freebsd" + ReleaseShort(DefaultRelease) + "-arm64-zfs.raw.zst"
	if p.URL() != want {
		t.Fatalf("URL %s\nwant %s", p.URL(), want)
	}
	if PrebakedFileName("15.1.0", "15.1-RELEASE") != "jailmachine-guest-15.1.0-freebsd15.1-arm64-zfs.raw.zst" {
		t.Fatal(PrebakedFileName("15.1.0", "15.1-RELEASE"))
	}
}

func TestGuestRelease(t *testing.T) {
	cases := map[string]string{
		"15.1.0": "15.1-RELEASE", // table
		"15.2.3": "15.2-RELEASE", // derived
		"16.0.0": "16.0-RELEASE",
		"junk":   DefaultRelease,
	}
	for ver, want := range cases {
		if got := GuestRelease(ver); got != want {
			t.Errorf("GuestRelease(%q) = %q, want %q", ver, got, want)
		}
	}
	if GuestRelease(GuestVersion) != guestReleases[GuestVersion] {
		t.Error("the default guest version must be in the table")
	}
	p := &Prebaked{Version: "16.0.0"}
	if !strings.HasSuffix(p.URL(), "/guest-16.0.0/jailmachine-guest-16.0.0-freebsd16.0-arm64-zfs.raw.zst") {
		t.Errorf("release not derived from the version: %s", p.URL())
	}
}

// prebakedServer serves a release directory with the archive and its
// sidecar; sidecar may be overridden to simulate mismatches.
func prebakedServer(t testing.TB, archive []byte, sidecar *string) (*Prebaked, *httptest.Server) {
	t.Helper()
	p := &Prebaked{Version: "9.9.9", Release: "15.1-RELEASE"}
	mux := http.NewServeMux()
	mux.HandleFunc("/guest-9.9.9/"+p.FileName(), func(rw http.ResponseWriter, r *http.Request) {
		http.ServeContent(rw, r, "img.zst", time0, bytes.NewReader(archive))
	})
	mux.HandleFunc("/guest-9.9.9/"+p.FileName()+".sha256", func(rw http.ResponseWriter, _ *http.Request) {
		if sidecar != nil {
			io.WriteString(rw, *sidecar)
			return
		}
		io.WriteString(rw, SidecarLine(sum(archive), p.FileName()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.BaseURL = srv.URL
	p.Client = srv.Client()
	return p, srv
}

func TestPrebakedFetchSparse(t *testing.T) {
	payload := sparsePayload()
	p, _ := prebakedServer(t, zstdBytes(t, payload), nil)
	p.DiskGiB = 1
	dest := filepath.Join(t.TempDir(), "disk.raw")
	var log bytes.Buffer
	if err := p.Fetch(context.Background(), dest, &log); err != nil {
		t.Fatalf("fetch: %v\n%s", err, log.String())
	}
	if size(t, dest) != GiB {
		t.Fatalf("size %d, want %d", size(t, dest), GiB)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:len(payload)], payload) || !bytes.Equal(got[len(payload):], make([]byte, GiB-int64(len(payload)))) {
		t.Fatal("payload differs after zstd round trip")
	}
	// Only ~32 KiB of data was written; the rest must be holes.
	if a := allocated(t, dest); a > 4<<20 {
		t.Fatalf("file not sparse: %d bytes allocated", a)
	}
	for _, leftover := range []string{dest + ".zst", dest + ".zst.part", dest + ".tmp"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed", leftover)
		}
	}
}

func TestPrebakedSidecarMismatch(t *testing.T) {
	bad := strings.Repeat("0", 64) + "  jailmachine-guest-9.9.9-freebsd15.1-arm64-zfs.raw.zst\n"
	p, _ := prebakedServer(t, zstdBytes(t, []byte("payload")), &bad)
	dest := filepath.Join(t.TempDir(), "disk.raw")
	err := p.Fetch(context.Background(), dest, nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("dest must not exist after a checksum failure")
	}

	wrongName := sum([]byte("x")) + "  somethingelse.zst\n"
	p, _ = prebakedServer(t, zstdBytes(t, []byte("payload")), &wrongName)
	if err := p.Fetch(context.Background(), dest, nil); !errors.Is(err, ErrSidecar) {
		t.Fatalf("want ErrSidecar, got %v", err)
	}
}

func TestPrebakedMissingSidecarFails(t *testing.T) {
	p, srv := prebakedServer(t, zstdBytes(t, []byte("payload")), nil)
	p.BaseURL = srv.URL + "/nope"
	err := p.Fetch(context.Background(), filepath.Join(t.TempDir(), "d"), nil)
	if !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("prebaked without a sidecar must fail with ErrNoChecksum, got %v", err)
	}
}

func TestSparseWriterRoundTrip(t *testing.T) {
	payload := sparsePayload()
	dest := filepath.Join(t.TempDir(), "sparse")
	n, err := copySparse(context.Background(), bytes.NewReader(payload), dest, nil, "copy")
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(payload)) || size(t, dest) != int64(len(payload)) {
		t.Fatalf("size %d / %d, want %d", n, size(t, dest), len(payload))
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, payload) {
		t.Fatal("content differs")
	}
	if a := allocated(t, dest); a > 4<<20 {
		t.Fatalf("not sparse: %d allocated", a)
	}
}

func TestBYOLocalRaw(t *testing.T) {
	dir := t.TempDir()
	payload := sparsePayload()
	src := filepath.Join(dir, "my.raw")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	b := &BYO{Ref: src, DiskGiB: 1}
	dest := filepath.Join(dir, "out", "disk.raw")
	if err := b.Fetch(context.Background(), dest, nil); err != nil {
		t.Fatal(err)
	}
	if b.Trusted() {
		t.Fatal("no sidecar: must be untrusted")
	}
	if size(t, dest) != GiB {
		t.Fatalf("size %d", size(t, dest))
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got[:len(payload)], payload) {
		t.Fatal("content differs")
	}
	if a := allocated(t, dest); a > 4<<20 {
		t.Fatalf("not sparse: %d allocated", a)
	}

	// With a sidecar it becomes trusted; a wrong sidecar fails.
	if err := os.WriteFile(src+".sha256", []byte(SidecarLine(sum(payload), src)), 0o644); err != nil {
		t.Fatal(err)
	}
	b = &BYO{Ref: src}
	if err := b.Fetch(context.Background(), filepath.Join(dir, "d2"), nil); err != nil || !b.Trusted() {
		t.Fatalf("trusted fetch: %v, trusted=%v", err, b.Trusted())
	}
	if err := os.WriteFile(src+".sha256", []byte(SidecarLine(strings.Repeat("1", 64), src)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (&BYO{Ref: src}).Fetch(context.Background(), filepath.Join(dir, "d3"), nil); !errors.Is(err, ErrChecksum) {
		t.Fatalf("want ErrChecksum, got %v", err)
	}
}

func TestBYOLocalCompressed(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("byo "), 8192)
	for name, data := range map[string][]byte{
		"img.raw.xz":  xzBytes(t, payload),
		"img.raw.zst": zstdBytes(t, payload),
	} {
		src := filepath.Join(dir, name)
		if err := os.WriteFile(src, data, 0o644); err != nil {
			t.Fatal(err)
		}
		b := &BYO{Ref: src, NoXZBinary: true}
		dest := filepath.Join(dir, name+".out")
		if err := b.Fetch(context.Background(), dest, nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := os.ReadFile(dest)
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s: content differs", name)
		}
	}
	if err := (&BYO{Ref: filepath.Join(dir, "img.qcow2")}).Fetch(context.Background(), filepath.Join(dir, "q"), nil); !errors.Is(err, ErrUnsupportedImage) {
		t.Fatalf("want ErrUnsupportedImage, got %v", err)
	}
}

func TestBYOURL(t *testing.T) {
	payload := bytes.Repeat([]byte("remote "), 4096)
	archive := zstdBytes(t, payload)
	var withSidecar bool
	mux := http.NewServeMux()
	mux.HandleFunc("/images/disk.raw.zst", func(rw http.ResponseWriter, r *http.Request) {
		http.ServeContent(rw, r, "disk.raw.zst", time0, bytes.NewReader(archive))
	})
	mux.HandleFunc("/images/disk.raw.zst.sha256", func(rw http.ResponseWriter, _ *http.Request) {
		if !withSidecar {
			http.NotFound(rw, nil)
			return
		}
		io.WriteString(rw, SidecarLine(sum(archive), "disk.raw.zst"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	b := &BYO{Ref: srv.URL + "/images/disk.raw.zst", Client: srv.Client()}
	if err := b.Fetch(context.Background(), filepath.Join(dir, "a"), nil); err != nil {
		t.Fatal(err)
	}
	if b.Trusted() {
		t.Fatal("404 sidecar must leave the image untrusted")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a"))
	if !bytes.Equal(got, payload) {
		t.Fatal("content differs")
	}
	withSidecar = true
	b = &BYO{Ref: srv.URL + "/images/disk.raw.zst", Client: srv.Client()}
	if err := b.Fetch(context.Background(), filepath.Join(dir, "b"), nil); err != nil || !b.Trusted() {
		t.Fatalf("with sidecar: %v, trusted=%v", err, b.Trusted())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestIsBYORef(t *testing.T) {
	for _, yes := range []string{"/x/disk.raw", "./disk.raw.xz", "~/img.raw.zst", "https://h/x.raw.zst", "disk.raw", "foo.img"} {
		if !IsBYORef(yes) {
			t.Errorf("%q should be BYO", yes)
		}
	}
	for _, no := range []string{"official", "official:15.1-RELEASE", "prebaked", "prebaked:15.1.0", ""} {
		if IsBYORef(no) {
			t.Errorf("%q should not be BYO", no)
		}
	}
}

var (
	_ Source = (*Prebaked)(nil)
	_ Source = (*BYO)(nil)
	_ Trust  = (*BYO)(nil)
)

// TestPrebakedBaseURLEnv: $JM_IMAGE_BASEURL points at a flat directory
// (no guest-<version>/ subdirectory) and is ignored once BaseURL is set.
func TestPrebakedBaseURLEnv(t *testing.T) {
	t.Setenv(BaseURLEnv, "http://127.0.0.1:1/flat/")
	p := &Prebaked{Version: "9.9.9", Release: "15.1-RELEASE"}
	if got, want := p.URL(), "http://127.0.0.1:1/flat/"+p.FileName(); got != want {
		t.Fatalf("URL %s\nwant %s", got, want)
	}
	p.BaseURL = "http://example.invalid/rel"
	if got, want := p.URL(), "http://example.invalid/rel/guest-9.9.9/"+p.FileName(); got != want {
		t.Fatalf("URL with BaseURL %s\nwant %s", got, want)
	}

	// End to end against a flat directory: sidecar verified, image installed.
	archive := zstdBytes(t, []byte("flat-image"))
	mux := http.NewServeMux()
	q := &Prebaked{Version: "9.9.9", Release: "15.1-RELEASE"}
	mux.HandleFunc("/"+q.FileName(), func(rw http.ResponseWriter, r *http.Request) {
		http.ServeContent(rw, r, "img.zst", time0, bytes.NewReader(archive))
	})
	mux.HandleFunc("/"+q.FileName()+".sha256", func(rw http.ResponseWriter, _ *http.Request) {
		io.WriteString(rw, SidecarLine(sum(archive), q.FileName()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv(BaseURLEnv, srv.URL)
	q.Client = srv.Client()
	dest := filepath.Join(t.TempDir(), "disk.raw")
	if err := q.Fetch(context.Background(), dest, nil); err != nil {
		t.Fatalf("fetch via %s: %v", BaseURLEnv, err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "flat-image" {
		t.Fatalf("installed image %q", got)
	}
}
