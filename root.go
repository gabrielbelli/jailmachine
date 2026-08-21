// Package jailmachine exposes repository-level assets that are embedded into
// the jm binary. It lives at the module root because go:embed cannot reach
// outside a package's directory and guest/provision.sh must stay the single
// source of truth for provisioning (ADR 0003).
package jailmachine

import _ "embed"

// ProvisionScript is the contents of guest/provision.sh, the first-boot
// provisioning script shipped to the guest on the NoCloud seed.
//
//go:embed guest/provision.sh
var ProvisionScript string

// SealScript is the contents of guest/seal.sh, run over ssh by "jm image
// build" to strip per-machine state from a provisioned guest before its
// disk is published as a prebaked image.
//
//go:embed guest/seal.sh
var SealScript string
