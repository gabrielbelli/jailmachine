package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/machine"
	"github.com/gabrielbelli/jailmachine/internal/sshx"
)

// sshdTimeout bounds the guest sshd configuration step.
const sshdTimeout = 20 * time.Second

// sshdSettings are the guest sshd lines jm relies on, as provision.sh writes
// them on a new disk:
//
//   - MaxAuthTries 20: podman-remote's ssh:// client (jpodman, the port
//     forwarder) offers the host's ssh-agent keys as well as the machine key,
//     and sshd's default of 6 tries runs out first.
//   - ClientAliveInterval 30 and ClientAliveCountMax 4: sshd ends a session
//     whose client has gone (a host that slept, a killed tunnel) within two
//     minutes, so a dead session stops holding an idle machine awake
//     (ADR 0009).
var sshdSettings = []string{"MaxAuthTries 20", "ClientAliveInterval 30", "ClientAliveCountMax 4"}

// sshdSettingsScript applies sshdSettings to the guest sshd. provision.sh runs
// once per disk, so a machine whose disk predates a line gets it from here.
// The program does nothing when every line is in place, keeps a copy it puts
// back when "sshd -t" rejects the edit, reloads sshd (host keys exist by now,
// and open sessions are not affected) and prints "changed" when it changed
// something.
var sshdSettingsScript = func() string {
	var have, set []string
	for _, kv := range sshdSettings {
		k, v, _ := strings.Cut(kv, " ")
		have = append(have, "grep -qx '"+kv+"' \"$f\"")
		set = append(set, "jm_set "+k+" "+v)
	}
	return `f=/etc/ssh/sshd_config
` + strings.Join(have, " && ") + ` && exit 0
jm_set() {
  grep -qx "$1 $2" "$f" && return 0
  sed -i '' -e "s/^#\{0,1\}$1.*/$1 $2/" "$f" &&
    { grep -qx "$1 $2" "$f" || echo "$1 $2" >> "$f"; }
}
cp -p "$f" "$f.jm" || exit
` + strings.Join(set, " &&\n  ") + ` &&
  sshd -t || { rc=$?; mv "$f.jm" "$f"; exit $rc; }
rm -f "$f.jm"
service sshd reload >/dev/null || exit
echo changed
`
}()

// syncGuestSSHD is the sshd step "jm start" runs once the guest is
// provisioned. A failure is a warning: jm's own connection does not need it.
func syncGuestSSHD(ctx context.Context, m *machine.Machine, client *sshx.Client) {
	ctx, cancel := context.WithTimeout(ctx, sshdTimeout)
	defer cancel()
	out, errOut, err := client.RunScript(ctx, "configuring the guest sshd", sshdSettingsScript)
	if err != nil {
		if msg := strings.TrimSpace(errOut + "\n" + out); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		fmt.Fprintf(stderr, "jm: warning: %s: %v; continuing\n", m.Name, err)
		return
	}
	if strings.Contains(out, "changed") {
		logf(stdout, "%s: set the guest sshd's %s", machine.StageProvision, strings.Join(sshdSettings, ", "))
	}
}
