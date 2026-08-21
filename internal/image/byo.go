package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Trust is implemented by sources whose integrity check is optional.
// Trusted reports, after Fetch, whether the image was verified against a
// published checksum; the machine record carries the answer to inspect.
type Trust interface {
	Trusted() bool
}

// ErrUnsupportedImage is returned for a BYO reference whose extension is
// not .raw, .xz or .zst.
var ErrUnsupportedImage = errors.New("image: unsupported image format (want .raw, .raw.xz or .raw.zst)")

// IsBYORef reports whether an --image value is a bring-your-own path or
// URL rather than a named source: a local path (absolute, relative with a
// directory part, or ~/...) or an http(s) URL, or anything ending in a
// known image extension.
func IsBYORef(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "http://"), strings.HasPrefix(ref, "https://"):
		return true
	case strings.HasPrefix(ref, "/"), strings.HasPrefix(ref, "./"), strings.HasPrefix(ref, "../"), strings.HasPrefix(ref, "~/"):
		return true
	}
	_, err := byoKind(ref)
	return err == nil
}

// byoKind returns the compression of a reference by extension: "" (raw),
// "xz" or "zst".
func byoKind(ref string) (string, error) {
	switch strings.ToLower(path.Ext(strings.TrimRight(ref, "/"))) {
	case ".raw", ".img":
		return "", nil
	case ".xz":
		return "xz", nil
	case ".zst":
		return "zst", nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnsupportedImage, ref)
}

// BYO is a bring-your-own image: a local path or an http(s) URL to a raw
// disk, optionally xz- or zstd-compressed. A sibling "<file>.sha256"
// (sidecar format) is used when present; without one the image is
// installed anyway and Trusted reports false.
type BYO struct {
	// Ref is the path or URL as the user gave it.
	Ref string
	// DiskGiB is the size the raw image is grown to.
	DiskGiB int
	// Client overrides http.DefaultClient for URLs.
	Client *http.Client
	// XZBinary / NoXZBinary are as on Official.
	XZBinary   string
	NoXZBinary bool

	trusted bool
}

// Name implements Source.
func (b *BYO) Name() string { return "byo" }

// Trusted implements Trust; meaningful after Fetch.
func (b *BYO) Trusted() bool { return b.trusted }

func (b *BYO) isURL() bool {
	return strings.HasPrefix(b.Ref, "http://") || strings.HasPrefix(b.Ref, "https://")
}

func (b *BYO) client() *http.Client {
	if b.Client != nil {
		return b.Client
	}
	return http.DefaultClient
}

func (b *BYO) xzBinary() string {
	if b.NoXZBinary {
		return ""
	}
	if b.XZBinary != "" {
		return b.XZBinary
	}
	return lookXZ()
}

// localPath expands a leading ~ and makes the reference absolute.
func (b *BYO) localPath() (string, error) {
	p := b.Ref
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("image: %w", err)
		}
		p = filepath.Join(home, p[2:])
	}
	return filepath.Abs(p)
}

// Fetch implements Source. The source file (downloaded to dest+".<ext>"
// for URLs, read in place for paths) is checked against its sidecar when
// one exists, then decompressed or copied sparsely into dest.
func (b *BYO) Fetch(ctx context.Context, dest string, progress io.Writer) error {
	if progress == nil {
		progress = io.Discard
	}
	logf := func(format string, args ...any) { fmt.Fprintf(progress, format+"\n", args...) }
	kind, err := byoKind(b.Ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("image: %w", err)
	}

	var src string
	var cleanup bool
	b.trusted = false
	if b.isURL() {
		file := path.Base(b.Ref)
		want, err := fetchSidecar(ctx, b.client(), b.Ref+".sha256", file)
		switch {
		case errors.Is(err, ErrNoChecksum):
			logf("no %s.sha256 published: image will be marked untrusted", file)
		case err != nil:
			return err
		}
		src = dest + ".byo" + path.Ext(file)
		cleanup = true
		if want != "" {
			if err := fetchVerified(ctx, b.client(), b.Ref, src, want, progress); err != nil {
				return err
			}
			b.trusted = true
		} else {
			part := src + ".part"
			logf("downloading %s", b.Ref)
			if err := download(ctx, b.client(), b.Ref, part, barWriter(progress)); err != nil {
				return err
			}
			if err := os.Rename(part, src); err != nil {
				return fmt.Errorf("image: %w", err)
			}
		}
	} else {
		src, err = b.localPath()
		if err != nil {
			return err
		}
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("image: %w", err)
		}
		sidecar, err := os.Open(src + ".sha256")
		switch {
		case err == nil:
			want, perr := ParseSidecar(sidecar, src)
			sidecar.Close()
			if perr != nil {
				return perr
			}
			logf("verifying SHA256 against %s", src+".sha256")
			got, herr := sha256File(src)
			if herr != nil {
				return fmt.Errorf("image: hashing %s: %w", src, herr)
			}
			if got != want {
				return fmt.Errorf("%w: %s: want %s, got %s", ErrChecksum, src, want, got)
			}
			b.trusted = true
		case os.IsNotExist(err):
			logf("no %s.sha256 next to the image: image will be marked untrusted", filepath.Base(src))
		default:
			return fmt.Errorf("image: %w", err)
		}
	}

	err = installRaw(ctx, dest, b.DiskGiB, func(tmp string) error {
		switch kind {
		case "xz":
			logf("decompressing xz")
			return decompressXZ(ctx, b.xzBinary(), src, tmp, barWriter(progress))
		case "zst":
			logf("decompressing zstd (sparse)")
			return decompressZstd(ctx, src, tmp, barWriter(progress))
		default:
			logf("copying %s (sparse)", src)
			in, err := os.Open(src)
			if err != nil {
				return fmt.Errorf("image: %w", err)
			}
			defer in.Close()
			_, err = copySparse(ctx, in, tmp, barWriter(progress), "copying")
			return err
		}
	})
	if err != nil {
		return err
	}
	if cleanup {
		_ = os.Remove(src)
	}
	logf("image ready: %s", dest)
	return nil
}
