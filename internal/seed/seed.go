// Package seed builds the NoCloud first-boot ISO (volume label "cidata")
// consumed by nuageinit: meta-data plus a #!/bin/sh user-data assembled
// from guest/provision.sh with JM_* variables prepended.
package seed

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kdomanski/iso9660"
)

// VolumeID is the ISO volume identifier nuageinit looks for.
const VolumeID = "cidata"

// File names inside the seed image.
const (
	MetaDataFile = "meta-data"
	UserDataFile = "user-data"
)

// Params describes the seed contents.
type Params struct {
	// InstanceID becomes the NoCloud instance-id (normally the machine name).
	InstanceID string
	// Hostname is applied by the provisioning script via JM_HOSTNAME.
	Hostname string
	// SSHPubKey is the authorised key installed for the SSH user. It must not
	// contain a single quote, as it is embedded in a single-quoted shell
	// assignment.
	SSHPubKey string
	// ProvisionScript is the body of guest/provision.sh. A leading shebang
	// line, if present, is stripped so that user-data starts with #!/bin/sh.
	ProvisionScript string
}

// ErrInvalidParams is wrapped by all parameter validation failures.
var ErrInvalidParams = errors.New("seed: invalid parameters")

// Validate checks that the parameters can be embedded safely.
func (p Params) Validate() error {
	switch {
	case p.InstanceID == "":
		return fmt.Errorf("%w: instance id is empty", ErrInvalidParams)
	case strings.ContainsAny(p.InstanceID, "\r\n"):
		return fmt.Errorf("%w: instance id contains a newline", ErrInvalidParams)
	case p.Hostname == "":
		return fmt.Errorf("%w: hostname is empty", ErrInvalidParams)
	case strings.ContainsAny(p.Hostname, "'\r\n"):
		return fmt.Errorf("%w: hostname contains a quote or newline", ErrInvalidParams)
	case p.SSHPubKey == "":
		return fmt.Errorf("%w: ssh public key is empty", ErrInvalidParams)
	case strings.ContainsAny(p.SSHPubKey, "'\r\n"):
		return fmt.Errorf("%w: ssh public key contains a single quote or newline", ErrInvalidParams)
	case p.ProvisionScript == "":
		return fmt.Errorf("%w: provision script is empty", ErrInvalidParams)
	}
	return nil
}

// MetaData renders the NoCloud meta-data file.
func MetaData(p Params) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", p.InstanceID, p.Hostname)
}

// UserData renders the NoCloud user-data file: a #!/bin/sh script that
// exports the JM_* variables and then runs the provisioning script.
func UserData(p Params) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "JM_SSH_PUBKEY='%s'\n", strings.TrimSpace(p.SSHPubKey))
	fmt.Fprintf(&b, "JM_HOSTNAME='%s'\n", p.Hostname)
	b.WriteString("export JM_SSH_PUBKEY JM_HOSTNAME\n")
	b.WriteString(stripShebang(p.ProvisionScript))
	return b.String()
}

// stripShebang removes a leading "#!" line so the script's own interpreter
// line cannot end up in the middle of user-data.
func stripShebang(script string) string {
	if !strings.HasPrefix(script, "#!") {
		return script
	}
	if i := strings.IndexByte(script, '\n'); i >= 0 {
		return script[i+1:]
	}
	return ""
}

// Build writes a NoCloud seed ISO to dest. The file is written atomically
// via a temporary sibling and renamed into place.
func Build(dest string, p Params) error {
	if err := p.Validate(); err != nil {
		return err
	}

	w, err := iso9660.NewWriter()
	if err != nil {
		return fmt.Errorf("seed: create iso writer: %w", err)
	}
	defer w.Cleanup() //nolint:errcheck // best-effort temp cleanup

	if err := w.AddFile(strings.NewReader(MetaData(p)), MetaDataFile); err != nil {
		return fmt.Errorf("seed: add %s: %w", MetaDataFile, err)
	}
	if err := w.AddFile(strings.NewReader(UserData(p)), UserDataFile); err != nil {
		return fmt.Errorf("seed: add %s: %w", UserDataFile, err)
	}

	var buf bytes.Buffer
	if err := w.WriteTo(&buf, VolumeID); err != nil {
		return fmt.Errorf("seed: write iso: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".seed-*.iso")
	if err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("seed: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("seed: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		cleanup()
		return fmt.Errorf("seed: %w", err)
	}
	return nil
}
