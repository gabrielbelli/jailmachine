package image

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// GuestVersion is the prebaked guest image version "jm init" fetches by
// default. Bump it manually when a new guest image is published with
// "jm image build" (the release tag is "guest-<GuestVersion>").
const GuestVersion = "15.1.0"

// guestReleases maps a published guest image version to the FreeBSD
// release it was built from (part of the file name). Add a row when a guest
// image is published; versions not listed derive "<major>.<minor>-RELEASE"
// from their first two components.
var guestReleases = map[string]string{
	"15.1.0": "15.1-RELEASE",
}

// GuestRelease returns the FreeBSD release baked into guest image version.
func GuestRelease(version string) string {
	if rel, ok := guestReleases[version]; ok {
		return rel
	}
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return DefaultRelease
	}
	return parts[0] + "." + parts[1] + "-RELEASE"
}

// DefaultReleaseBaseURL is where prebaked guest images are published.
const DefaultReleaseBaseURL = "https://github.com/gabrielbelli/jailmachine/releases/download"

// BaseURLEnv names the environment variable that points the prebaked source
// at a flat directory instead of GitHub releases: the image is fetched from
// "$JM_IMAGE_BASEURL/<file name>" (and its sidecar from the same place with
// ".sha256" appended). Meant for testing a freshly built image before it is
// published, e.g. JM_IMAGE_BASEURL=http://127.0.0.1:8000 over "dist/".
const BaseURLEnv = "JM_IMAGE_BASEURL"

// ErrSidecar is returned when a .sha256 sidecar cannot be parsed or names
// a different file.
var ErrSidecar = errors.New("image: invalid .sha256 sidecar")

var sidecarHash = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// ParseSidecar reads a "<hash>  <filename>" sidecar (sha256sum/shasum
// output) and returns the lower-case digest. A filename, when present,
// must match file (a leading "*" for binary mode is tolerated); a bare
// hash is accepted.
func ParseSidecar(r io.Reader, file string) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if !sidecarHash.MatchString(fields[0]) {
			return "", fmt.Errorf("%w: %q", ErrSidecar, line)
		}
		if len(fields) > 1 {
			name := strings.TrimPrefix(fields[1], "*")
			if path.Base(name) != path.Base(file) {
				return "", fmt.Errorf("%w: names %q, want %q", ErrSidecar, name, file)
			}
		}
		return strings.ToLower(fields[0]), nil
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("image: reading sidecar: %w", err)
	}
	return "", fmt.Errorf("%w: empty", ErrSidecar)
}

// SidecarLine renders the sidecar contents for file with digest sum.
func SidecarLine(sum, file string) string { return sum + "  " + path.Base(file) + "\n" }

// fetchSidecar downloads url (a .sha256 file) and returns the digest it
// publishes for file. A 404 is reported as ErrNoChecksum so callers can
// decide whether an unverifiable image is acceptable.
func fetchSidecar(ctx context.Context, client *http.Client, url, file string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("image: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", ErrNoChecksum, url)
	default:
		return "", fmt.Errorf("image: GET %s: unexpected status %s", url, resp.Status)
	}
	return ParseSidecar(io.LimitReader(resp.Body, 4096), file)
}

// ReleaseShort turns "15.1-RELEASE" into "15.1", the form used in prebaked
// image file names.
func ReleaseShort(release string) string {
	return strings.TrimSuffix(release, "-RELEASE")
}

// PrebakedFileName is the basename of a prebaked image for a guest version
// and FreeBSD release, e.g. jailmachine-guest-15.1.0-freebsd15.1-arm64-zfs.raw.zst.
func PrebakedFileName(version, release string) string {
	return fmt.Sprintf("jailmachine-guest-%s-freebsd%s-arm64-zfs.raw.zst", version, ReleaseShort(release))
}

// Prebaked downloads a guest image already provisioned by "jm image build"
// from the project's GitHub releases. It is the fast path of ADR 0003: the
// seed only applies the SSH key and hostname on first boot. Integrity is a
// SHA256 sidecar next to the image (signatures come post-MVP).
type Prebaked struct {
	// Version is the guest image version (release tag "guest-<Version>").
	// Defaults to GuestVersion.
	Version string
	// Release is the FreeBSD release baked into the image, part of the file
	// name. Defaults to GuestRelease(Version).
	Release string
	// BaseURL overrides DefaultReleaseBaseURL (used by tests); the release
	// directory "guest-<version>/" is still appended.
	BaseURL string
	// DiskGiB is the size the raw image is grown to after decompression.
	DiskGiB int
	// Client overrides http.DefaultClient.
	Client *http.Client
}

// Name implements Source.
func (p *Prebaked) Name() string { return "prebaked" }

func (p *Prebaked) version() string {
	if p.Version == "" {
		return GuestVersion
	}
	return p.Version
}

func (p *Prebaked) release() string {
	if p.Release == "" {
		return GuestRelease(p.version())
	}
	return p.Release
}

// FileName is the basename of the compressed image.
func (p *Prebaked) FileName() string { return PrebakedFileName(p.version(), p.release()) }

// URL is the full image URL; the sidecar is URL()+".sha256". With
// $JM_IMAGE_BASEURL set (and no explicit BaseURL) the file is expected
// directly under that URL, without the per-release directory.
func (p *Prebaked) URL() string {
	if p.BaseURL == "" {
		if env := os.Getenv(BaseURLEnv); env != "" {
			return strings.TrimRight(env, "/") + "/" + p.FileName()
		}
	}
	base := p.BaseURL
	if base == "" {
		base = DefaultReleaseBaseURL
	}
	return strings.TrimRight(base, "/") + "/guest-" + p.version() + "/" + p.FileName()
}

func (p *Prebaked) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// Fetch implements Source: fetch the sidecar, download (resuming) to
// dest+".zst.part", verify, decompress sparsely to dest, grow to DiskGiB.
func (p *Prebaked) Fetch(ctx context.Context, dest string, progress io.Writer) error {
	if progress == nil {
		progress = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(progress, format+"\n", args...) }
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	want, err := fetchSidecar(ctx, p.client(), p.URL()+".sha256", p.FileName())
	if err != nil {
		return err
	}
	archive := dest + ".zst"
	if err := fetchVerified(ctx, p.client(), p.URL(), archive, want, progress); err != nil {
		return err
	}
	logf("decompressing (sparse)")
	if err := installRaw(ctx, dest, p.DiskGiB, func(tmp string) error {
		return decompressZstd(ctx, archive, tmp, barWriter(progress))
	}); err != nil {
		return err
	}
	if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("image: %w", err)
	}
	logf("image ready: %s", dest)
	return nil
}

// fetchVerified leaves a download of url at archive whose SHA256 is want.
// A verified archive left by an earlier run is reused; otherwise the
// download resumes into archive+".part" and is renamed once it checks out.
func fetchVerified(ctx context.Context, client *http.Client, url, archive, want string, progress io.Writer) error {
	logf := func(format string, args ...any) { fmt.Fprintf(progress, format+"\n", args...) }
	if _, err := os.Stat(archive); err == nil {
		logf("reusing %s", archive)
		return verify(archive, want)
	}
	part := archive + ".part"
	logf("downloading %s", url)
	if err := download(ctx, client, url, part, barWriter(progress)); err != nil {
		return err
	}
	logf("verifying SHA256")
	if err := verify(part, want); err != nil {
		return err
	}
	if err := os.Rename(part, archive); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	return nil
}

// installRaw produces dest atomically: produce writes the raw disk to a
// temporary sibling, which is grown to diskGiB and renamed into place.
func installRaw(ctx context.Context, dest string, diskGiB int, produce func(tmp string) error) error {
	tmp := dest + ".tmp"
	if err := produce(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if diskGiB > 0 {
		if err := Grow(tmp, int64(diskGiB)*GiB); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("image: %w", err)
	}
	return nil
}
