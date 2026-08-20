package image

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRelease is the FreeBSD release fetched when none is given.
const DefaultRelease = "15.1-RELEASE"

// DefaultBaseURL is the directory under which release VM images live.
const DefaultBaseURL = "https://download.freebsd.org/releases/VM-IMAGES"

// Official downloads the upstream FreeBSD BASIC-CLOUDINIT ZFS image for
// arm64. It is the slow path of ADR 0003: a stock disk that the seed
// provisions fully on first boot.
type Official struct {
	// Release such as "15.1-RELEASE". Defaults to DefaultRelease.
	Release string
	// BaseURL overrides DefaultBaseURL (used by tests).
	BaseURL string
	// DiskGiB is the size the raw image is grown to after decompression.
	// Zero leaves the image at its shipped size.
	DiskGiB int
	// Client overrides http.DefaultClient.
	Client *http.Client
	// XZBinary is the external decompressor to use. Empty means "look for
	// xz on PATH"; set NoXZBinary to force the in-process decoder.
	XZBinary string
	// NoXZBinary forces the pure-Go decoder even when xz is installed.
	NoXZBinary bool
}

// Name implements Source.
func (o *Official) Name() string { return "official" }

func (o *Official) release() string {
	if o.Release == "" {
		return DefaultRelease
	}
	return o.Release
}

// FileName is the basename of the compressed image, e.g.
// FreeBSD-15.1-RELEASE-arm64-aarch64-BASIC-CLOUDINIT-zfs.raw.xz.
func (o *Official) FileName() string {
	return fmt.Sprintf("FreeBSD-%s-arm64-aarch64-BASIC-CLOUDINIT-zfs.raw.xz", o.release())
}

// DirURL is the directory holding the image and its CHECKSUM.SHA256.
func (o *Official) DirURL() string {
	base := o.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimRight(base, "/") + "/" + o.release() + "/aarch64/Latest"
}

// URL is the full image URL.
func (o *Official) URL() string { return o.DirURL() + "/" + o.FileName() }

func (o *Official) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return http.DefaultClient
}

func (o *Official) xzBinary() string {
	if o.NoXZBinary {
		return ""
	}
	if o.XZBinary != "" {
		return o.XZBinary
	}
	return lookXZ()
}

// Fetch implements Source. Steps: fetch the published checksum, download
// (resuming) to dest+".xz.part", verify, decompress to dest, grow to
// DiskGiB, remove the archive.
func (o *Official) Fetch(ctx context.Context, dest string, progress io.Writer) error {
	if progress == nil {
		progress = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(progress, format+"\n", args...) }

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	file := o.FileName()
	archive := dest + ".xz"
	part := archive + ".part"

	want, err := fetchChecksum(ctx, o.client(), o.DirURL()+"/CHECKSUM.SHA256", file)
	if err != nil {
		return err
	}

	// A verified archive left by an earlier run that failed during
	// decompression is reused as is.
	if _, err := os.Stat(archive); err != nil {
		logf("downloading %s", o.URL())
		if err := download(ctx, o.client(), o.URL(), part, barWriter(progress)); err != nil {
			return err
		}
		logf("verifying SHA256")
		if err := verify(part, want); err != nil {
			return err
		}
		if err := os.Rename(part, archive); err != nil {
			return fmt.Errorf("image: %w", err)
		}
	} else {
		logf("reusing %s", archive)
		if err := verify(archive, want); err != nil {
			return err
		}
	}

	tmp := dest + ".tmp"
	if bin := o.xzBinary(); bin != "" {
		logf("decompressing with %s", bin)
	} else {
		logf("decompressing (in-process; install xz for a faster path)")
	}
	if err := decompressXZ(ctx, o.xzBinary(), archive, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if o.DiskGiB > 0 {
		if err := Grow(tmp, int64(o.DiskGiB)*GiB); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("image: %w", err)
	}
	if err := os.Remove(archive); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("image: %w", err)
	}
	logf("image ready: %s", dest)
	return nil
}

// barWriter returns nil for io.Discard so download skips the progress
// bar entirely instead of rendering into the void.
func barWriter(w io.Writer) io.Writer {
	if w == io.Discard {
		return nil
	}
	return w
}
