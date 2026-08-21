//go:build !netgo

package resolver

// HostResolver reports whether this build reaches the host operating
// system's resolver (getaddrinfo through libSystem) rather than Go's own
// DNS client. Only the former honours scoped resolvers, /etc/hosts and
// .local, so "jm doctor" fails when it is false: losing them is invisible
// otherwise, because public names keep resolving (ADR 0008).
//
// On darwin it is true whatever CGO_ENABLED says: the standard library
// compiles the libSystem path unconditionally there (src/net/cgo_unix.go is
// "//go:build !netgo && ((cgo && unix) || darwin)", and cgo_stub.go excludes
// darwin outright). Only "-tags netgo" — or GODEBUG=netdns=go at run time —
// gives that up, so the tag here is the platform, not cgo.
const HostResolver = true
