package cli

import (
	"fmt"
	"os"

	"github.com/gabrielbelli/jailmachine/internal/forwarder"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// The host address published container ports bind to (ADR 0004). It is a
// machine property, not ambient state of the shell that happened to boot
// the machine: the forwarder runs detached, so an environment variable read
// inside it would be invisible to "jm inspect" and would change under the
// user the next time anyone ran a plain "jm start".

// publishAddrFlagUsage is shared by "jm init" and "jm set".
const publishAddrFlagUsage = "default host address published container ports bind to (default " +
	forwarder.DefaultHostIP + ", every interface, as docker does on Linux; 127.0.0.1 keeps them off the LAN). " +
	"A -p that names a host address of its own binds that address instead"

// parsePublishAddr validates a --publish-addr value as a usage error.
func parsePublishAddr(v string) (string, error) {
	addr, err := forwarder.ParsePublishAddr(v)
	if err != nil {
		return "", usagef("--publish-addr %v", err)
	}
	return addr, nil
}

// applyPublishAddrEnv folds $JM_PUBLISH_ADDR into the record at start time,
// so that the address the detached forwarder binds is the one recorded and
// shown. Without the variable the record stands; a bad value is a usage
// error here rather than a per-mapping expose failure later.
func applyPublishAddrEnv(m *machine.Machine) error {
	v, ok := os.LookupEnv(forwarder.PublishAddrEnv)
	if !ok {
		return nil
	}
	addr, err := forwarder.ParsePublishAddr(v)
	if err != nil {
		return usagef("$%s: %v", forwarder.PublishAddrEnv, err)
	}
	if addr == m.PublishAddr {
		return nil
	}
	logf(stdout, "publish address: %s -> %s (from $%s)",
		forwarder.HostIP(m.PublishAddr), forwarder.HostIP(addr), forwarder.PublishAddrEnv)
	m.PublishAddr = addr
	return store().Save(m)
}

// publishAddrs reports the publish address that is really in force and, when
// the record has been changed since the forwarder started, the one waiting
// for a restart. A running forwarder goes on binding what it booted with, so
// "jm ports" and "jm inspect" must not read the record back as fact.
func publishAddrs(m *machine.Machine, running bool, st *forwarder.State) (inForce, pending string) {
	record := forwarder.HostIP(m.PublishAddr)
	if running && st != nil && st.PublishAddr != "" && st.PublishAddr != record {
		return st.PublishAddr, record
	}
	return record, ""
}

// publishAddrNote is the line "jm set" prints when the address changed on a
// machine whose forwarder is already running with the old one.
func publishAddrNote(m *machine.Machine) string {
	return fmt.Sprintf("published ports bind %s from the next start: jm stop%s && jm start%s",
		forwarder.HostIP(m.PublishAddr), nameHint(m.Name), nameHint(m.Name))
}
