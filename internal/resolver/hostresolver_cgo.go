//go:build !darwin && cgo && !netgo

package resolver

// HostResolver is true off darwin only when cgo is available: there the
// libSystem-equivalent path (getaddrinfo through libc) needs it. See
// hostresolver_darwin.go.
const HostResolver = true
