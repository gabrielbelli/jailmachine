// Package image acquires and verifies guest disk images (ADR 0003): the
// official FreeBSD BASIC-CLOUDINIT .raw.xz, prebaked release artefacts, or
// bring-your-own paths. Download with resume, SHA256 check, xz
// decompression and sparse growth to the configured size.
package image

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// Source is an image provider: something that can place a raw disk image
// satisfying the guest contract at dest. Implementations: Official (M1);
// prebaked release artefacts and bring-your-own paths come later.
type Source interface {
	// Name is the short identifier recorded in machine.json ("official").
	Name() string
	// Fetch writes a ready-to-boot raw disk to dest. progress, when non-nil,
	// receives human-readable progress output (a progress bar or log lines).
	Fetch(ctx context.Context, dest string, progress io.Writer) error
}

// ErrChecksum is returned when a downloaded file does not match the
// published SHA256. The partial download is removed so the next attempt
// starts from scratch.
var ErrChecksum = errors.New("image: checksum mismatch")

// ErrNoChecksum is returned when CHECKSUM.SHA256 has no entry for the file.
var ErrNoChecksum = errors.New("image: file not listed in CHECKSUM.SHA256")

// GiB is one gibibyte in bytes.
const GiB = int64(1) << 30

// checksumLine matches the BSD-style "SHA256 (file) = hash" format used by
// FreeBSD's CHECKSUM.SHA256 files.
var checksumLine = regexp.MustCompile(`^SHA256 \((.+)\) = ([0-9a-fA-F]{64})$`)

// ParseChecksums reads a CHECKSUM.SHA256 file and returns a map of file
// name to lower-case hex digest. Lines that do not match the format are
// ignored, so comments and blank lines are harmless.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	sums := make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		m := checksumLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		sums[m[1]] = strings.ToLower(m[2])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("image: reading checksums: %w", err)
	}
	return sums, nil
}

// Grow sparsely extends the file at path to sizeBytes. It never shrinks:
// a file already at or above the requested size is left untouched.
func Grow(path string, sizeBytes int64) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("image: grow %s: %w", path, err)
	}
	if st.Size() >= sizeBytes {
		return nil
	}
	if err := os.Truncate(path, sizeBytes); err != nil {
		return fmt.Errorf("image: grow %s to %d bytes: %w", path, sizeBytes, err)
	}
	return nil
}
