package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/schollz/progressbar/v3"
)

// sparseBlock is the granularity at which runs of zeros become holes. It
// matches the common filesystem block size, so a hole is never smaller
// than what the filesystem can actually leave unallocated.
const sparseBlock = 4096

// sparseWriter writes to a file, skipping over all-zero blocks with Seek
// so that a mostly empty 64 GiB disk image stays a small sparse file.
// Close truncates the file to the logical length so trailing holes survive.
type sparseWriter struct {
	f   *os.File
	off int64
	// pending counts bytes of a zero run not yet materialised: they become
	// a hole (Seek) when non-zero data follows, or the final size on Close.
	pending int64
}

func newSparseWriter(f *os.File) *sparseWriter { return &sparseWriter{f: f} }

var zeroBlock = make([]byte, sparseBlock)

func (w *sparseWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		chunk := p
		if len(chunk) > sparseBlock {
			chunk = chunk[:sparseBlock]
		}
		p = p[len(chunk):]
		if bytes.Equal(chunk, zeroBlock[:len(chunk)]) {
			w.pending += int64(len(chunk))
			continue
		}
		if w.pending > 0 {
			if _, err := w.f.Seek(w.off+w.pending, io.SeekStart); err != nil {
				return 0, err
			}
			w.off += w.pending
			w.pending = 0
		}
		m, err := w.f.Write(chunk)
		w.off += int64(m)
		if err != nil {
			return 0, err
		}
	}
	return n, nil
}

// Size is the logical number of bytes written so far.
func (w *sparseWriter) Size() int64 { return w.off + w.pending }

// Close materialises a trailing zero run as file length and syncs. It does
// not close the underlying file.
func (w *sparseWriter) Close() error {
	if w.pending > 0 {
		if err := w.f.Truncate(w.off + w.pending); err != nil {
			return err
		}
		w.off += w.pending
		w.pending = 0
	}
	return w.f.Sync()
}

// copySparse copies r into a new file at dst, leaving holes where r
// yields zeros. An existing dst is truncated first.
func copySparse(ctx context.Context, r io.Reader, dst string, progress io.Writer, desc string) (int64, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("image: create %s: %w", dst, err)
	}
	defer out.Close()
	sw := newSparseWriter(out)

	var w io.Writer = sw
	if progress != nil {
		spin := progressbar.NewOptions64(-1,
			progressbar.OptionSetWriter(progress),
			progressbar.OptionSetDescription(desc),
			progressbar.OptionShowBytes(true),
			progressbar.OptionShowCount(),
			progressbar.OptionSetElapsedTime(true),
			progressbar.OptionSpinnerType(14),
			progressbar.OptionThrottle(250*time.Millisecond),
			progressbar.OptionOnCompletion(func() { fmt.Fprintln(progress) }),
		)
		defer func() { _ = spin.Finish() }()
		w = io.MultiWriter(sw, spin)
	}
	if _, err := io.Copy(w, &ctxReader{ctx: ctx, r: r}); err != nil {
		return 0, fmt.Errorf("image: writing %s: %w", dst, err)
	}
	if err := sw.Close(); err != nil {
		return 0, fmt.Errorf("image: finishing %s: %w", dst, err)
	}
	return sw.Size(), nil
}

// decompressZstd writes the decompressed contents of src to dst sparsely
// (zero blocks become holes), using the in-process klauspost decoder.
func decompressZstd(ctx context.Context, src, dst string, progress io.Writer) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("image: open %s: %w", src, err)
	}
	defer in.Close()
	dec, err := zstd.NewReader(in)
	if err != nil {
		return fmt.Errorf("image: %s: %w", src, err)
	}
	defer dec.Close()
	_, err = copySparse(ctx, dec, dst, progress, "decompressing")
	return err
}
