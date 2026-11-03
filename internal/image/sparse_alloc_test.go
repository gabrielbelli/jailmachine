package image

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestSparseWriterAllocatesOnlyData guards the APFS case: 1 MiB zero gaps
// between 4 KiB of data must not end up allocated on disk.
func TestSparseWriterAllocatesOnlyData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk.raw")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sw := newSparseWriter(f)
	data := bytes.Repeat([]byte{'x'}, sparseBlock)
	gap := make([]byte, 1<<20)
	for i := 0; i < 64; i++ {
		if _, err := sw.Write(data); err != nil {
			t.Fatal(err)
		}
		if _, err := sw.Write(gap); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no block count on this platform")
	}
	allocated := sys.Blocks * 512
	if logical := st.Size(); allocated > logical/4 {
		t.Fatalf("%d of %d bytes allocated; the zero gaps were not left as holes", allocated, logical)
	}
}
