package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// resolveDefault picks the machine a command without a name argument
// means, given the names that exist:
//
//   - if "jailmachine" exists, it wins (no note);
//   - otherwise, if exactly one machine exists, that one, with a note so
//     the user is not surprised;
//   - otherwise a usage error listing the candidates (none: "run jm init";
//     several: name one).
//
// It is pure so it can be unit-tested without a store.
func resolveDefault(existing []string) (name, note string, err error) {
	for _, n := range existing {
		if n == machine.DefaultName {
			return n, "", nil
		}
	}
	switch len(existing) {
	case 0:
		return "", "", withHint(fmt.Errorf("no machines exist"), "run 'jm init' to create "+machine.DefaultName)
	case 1:
		return existing[0], fmt.Sprintf("using %s (the only machine)", existing[0]), nil
	}
	names := append([]string(nil), existing...)
	sort.Strings(names)
	return "", "", usagef("no machine named %q and several exist: %s (name one, e.g. 'jm %s %s')",
		machine.DefaultName, strings.Join(names, ", "), activeCommand, names[0])
}

// resolveName turns the optional positional argument into an existing
// machine name, applying resolveDefault when it is absent. A note from
// the resolver is printed as a stage line. It records the name for error
// messages.
func resolveName(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		name, err := machine.ResolveName(args)
		if err != nil {
			return "", usage(err)
		}
		activeMachine = name
		return name, nil
	}
	ms, err := store().List()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.Name)
	}
	name, note, err := resolveDefault(names)
	if err != nil {
		return "", err
	}
	activeMachine = name
	if note != "" {
		logf(stdout, "%s", note)
	}
	return name, nil
}

// activeMachine and activeCommand are what the error formatter prints in
// "jm: <command> <name>: ..."; set as soon as they are known.
var (
	activeMachine string
	activeCommand string
)
