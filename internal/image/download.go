package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/ulikunitz/xz"
)

// download fetches url into part, resuming from whatever is already there.
// It returns nil when the file is complete on disk. A server that ignores
// the Range header (200 instead of 206) causes a restart from zero.
func download(ctx context.Context, client *http.Client, url, part string, progress io.Writer) error {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("image: open %s: %w", part, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("image: stat %s: %w", part, err)
	}
	have := st.Size()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("image: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	var total int64 = -1
	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored the Range (or nothing to resume): start over.
		have = 0
		if err := f.Truncate(0); err != nil {
			return fmt.Errorf("image: truncate %s: %w", part, err)
		}
		total = resp.ContentLength
	case http.StatusPartialContent:
		if resp.ContentLength >= 0 {
			total = have + resp.ContentLength
		}
	case http.StatusRequestedRangeNotSatisfiable:
		// Already fully downloaded; the checksum step decides whether it
		// is any good.
		return nil
	default:
		return fmt.Errorf("image: GET %s: unexpected status %s", url, resp.Status)
	}

	if _, err := f.Seek(have, io.SeekStart); err != nil {
		return fmt.Errorf("image: seek %s: %w", part, err)
	}

	var w io.Writer = f
	if progress != nil {
		// Renders as "downloading  42% |████    | (312/740 MB, 9.1 MB/s) [34s:47s]":
		// bytes done of total, rate, elapsed and ETA.
		bar := progressbar.NewOptions64(total,
			progressbar.OptionSetWriter(progress),
			progressbar.OptionSetDescription("downloading"),
			progressbar.OptionShowBytes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionShowTotalBytes(true),
			progressbar.OptionSetPredictTime(true),
			progressbar.OptionSetElapsedTime(true),
			progressbar.OptionShowElapsedTimeOnFinish(),
			progressbar.OptionThrottle(250*time.Millisecond),
			progressbar.OptionOnCompletion(func() { fmt.Fprintln(progress) }),
		)
		if have > 0 {
			_ = bar.Add64(have)
		}
		w = io.MultiWriter(f, bar)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("image: downloading %s: %w", url, err)
	}
	if total >= 0 {
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if st.Size() != total {
			return fmt.Errorf("image: %s: short download (%d of %d bytes)", part, st.Size(), total)
		}
	}
	return f.Sync()
}

// fetchChecksum downloads CHECKSUM.SHA256 from url and returns the digest
// published for file.
func fetchChecksum(ctx context.Context, client *http.Client, url, file string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("image: GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("image: GET %s: unexpected status %s", url, resp.Status)
	}
	sums, err := ParseChecksums(resp.Body)
	if err != nil {
		return "", err
	}
	want, ok := sums[file]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNoChecksum, file)
	}
	return want, nil
}

// sha256File returns the lower-case hex SHA256 of the file at path.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verify compares the file's SHA256 with want. On mismatch the file is
// removed and ErrChecksum returned.
func verify(path, want string) error {
	got, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("image: hashing %s: %w", path, err)
	}
	if got != want {
		_ = os.Remove(path)
		return fmt.Errorf("%w: %s: want %s, got %s", ErrChecksum, path, want, got)
	}
	return nil
}

// decompressXZ writes the decompressed contents of src to dst. When
// xzBinary is non-empty it is executed as "xz -dc src" (several times
// faster than the pure-Go decoder); otherwise ulikunitz/xz is used. When
// progress is non-nil a spinner with the bytes written so far is drawn on
// it (the decompressed size is not known up front, so there is no ETA).
func decompressXZ(ctx context.Context, xzBinary, src, dst string, progress io.Writer) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("image: create %s: %w", dst, err)
	}
	defer out.Close()

	var w io.Writer = out
	if progress != nil {
		spin := progressbar.NewOptions64(-1,
			progressbar.OptionSetWriter(progress),
			progressbar.OptionSetDescription("decompressing"),
			progressbar.OptionShowBytes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionSetElapsedTime(true),
			progressbar.OptionSpinnerType(14),
			progressbar.OptionThrottle(250*time.Millisecond),
			progressbar.OptionOnCompletion(func() { fmt.Fprintln(progress) }),
		)
		defer func() { _ = spin.Finish() }()
		w = io.MultiWriter(out, spin)
	}

	if xzBinary != "" {
		cmd := exec.CommandContext(ctx, xzBinary, "-dc", src)
		cmd.Stdout = w
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("image: %s -dc %s: %w", xzBinary, src, err)
		}
		return out.Sync()
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("image: open %s: %w", src, err)
	}
	defer in.Close()
	r, err := xz.NewReader(in)
	if err != nil {
		return fmt.Errorf("image: %s: %w", src, err)
	}
	if _, err := io.Copy(w, &ctxReader{ctx: ctx, r: r}); err != nil {
		return fmt.Errorf("image: decompressing %s: %w", src, err)
	}
	return out.Sync()
}

// ctxReader aborts a long copy when ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// lookXZ returns the path of an xz binary on PATH, or "" if none.
func lookXZ() string {
	p, err := exec.LookPath("xz")
	if err != nil || errors.Is(err, exec.ErrDot) {
		return ""
	}
	return p
}
