//go:build netgo || (!darwin && !cgo)

package resolver

// HostResolver is false when Go's own DNS client is the only resolver this
// build has: "-tags netgo" anywhere, or no cgo off darwin. See
// hostresolver_darwin.go.
const HostResolver = false
