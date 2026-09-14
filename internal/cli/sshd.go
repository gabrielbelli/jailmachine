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

// sshdMaxAuthTriesScript raises the guest sshd's MaxAuthTries to 20, as
// provision.sh does on a new disk. podman-remote's ssh:// client (jpodman,
// the port forwarder) offers the host's ssh-agent keys as well as the
// machine key, and sshd's default of 6 tries runs out first. provision.sh
// runs once per disk, so a machine whose disk predates that line gets it
// from here. The program does nothing when the line is in place, keeps a
// copy it puts back when "sshd -t" rejects the edit, reloads sshd (host keys
// exist by now, and open sessions are not affected) and prints "changed"
// when it changed something.
const sshdMaxAuthTriesScript = `f=/etc/ssh/sshd_config
grep -qx 'MaxAuthTries 20' "$f" && exit 0
cp -p "$f" "$f.jm" || exit
sed -i '' -e 's/^#\{0,1\}MaxAuthTries.*/MaxAuthTries 20/' "$f" &&
  { grep -qx 'MaxAuthTries 20' "$f" || echo 'MaxAuthTries 20' >> "$f"; } &&
  sshd -t || { rc=$?; mv "$f.jm" "$f"; exit $rc; }
rm -f "$f.jm"
service sshd reload >/dev/null || exit
echo changed
`

// syncGuestSSHD is the sshd step "jm start" runs once the guest is
// provisioned. A failure is a warning: jm's own connection does not need it.
func syncGuestSSHD(ctx context.Context, m *machine.Machine, client *sshx.Client) {
	ctx, cancel := context.WithTimeout(ctx, sshdTimeout)
	defer cancel()
	out, errOut, err := client.RunScript(ctx, "raising the guest sshd's MaxAuthTries", sshdMaxAuthTriesScript)
	if err != nil {
		if msg := strings.TrimSpace(errOut + "\n" + out); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		fmt.Fprintf(stderr, "jm: warning: %s: %v; continuing\n", m.Name, err)
		return
	}
	if strings.Contains(out, "changed") {
		logf(stdout, "%s: raised the guest sshd's MaxAuthTries to 20", machine.StageProvision)
	}
}
