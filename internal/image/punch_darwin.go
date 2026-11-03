package image

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fpunchhole mirrors struct fpunchhole from <sys/fcntl.h>.
type fpunchhole struct {
	flags    uint32
	reserved uint32
	offset   int64
	length   int64
}

// punchHole deallocates [off, off+n) of f. APFS zero-fills a gap left by a
// seek past end of file when the gap is shorter than a few MiB, so the
// sparse writer's holes would otherwise land on disk as real zero blocks:
// a 64 GiB guest disk with 3 GiB of content was measured at 62 GiB. The
// range is shrunk to whole filesystem blocks, which F_PUNCHHOLE requires.
// Best effort: a filesystem that refuses keeps the zeros, which is only
// wasted space.
func punchHole(f *os.File, off, n int64) {
	start := (off + sparseBlock - 1) / sparseBlock * sparseBlock
	end := (off + n) / sparseBlock * sparseBlock
	if end <= start {
		return
	}
	arg := fpunchhole{offset: start, length: end - start}
	_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), unix.F_PUNCHHOLE, uintptr(unsafe.Pointer(&arg)))
}
