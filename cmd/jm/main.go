// Command jm provisions and manages a FreeBSD virtual machine for running
// jails and OCI containers, and connects the host's podman client to it.
package main

import "github.com/gabrielbelli/jailmachine/internal/cli"

func main() {
	cli.Execute()
}
