//go:build !darwin

package image

import "os"

// punchHole is a no-op where a seek past end of file already leaves a
// hole (see punch_darwin.go for why macOS needs more).
func punchHole(*os.File, int64, int64) {}
