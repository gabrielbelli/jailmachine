//go:build deps

// Package main (under the "deps" build tag) pins the modules the sibling
// packages will import once they are filled in, so that "go mod tidy" keeps
// them in go.mod and go.sum before any real code uses them. It is never
// compiled into the binary.
package main

import (
	_ "github.com/gofrs/flock"
	_ "github.com/kdomanski/iso9660"
	_ "github.com/schollz/progressbar/v3"
	_ "github.com/spf13/cobra"
	_ "github.com/ulikunitz/xz"
	_ "golang.org/x/crypto/ssh"
)
