//go:build netgo

package resolver

// netgoBuild records whether this test binary was built with "-tags netgo",
// the one thing that really costs darwin its libSystem resolver.
const netgoBuild = true
