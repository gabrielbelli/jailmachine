package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend/qemu"
	"github.com/gabrielbelli/jailmachine/internal/image"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

func TestFetchHintPrebakedMissingRelease(t *testing.T) {
	ref := machine.ImageRef{Source: "prebaked", Release: image.GuestVersion}
	err := errors.Join(image.ErrNoChecksum)
	h := fetchHint("dev", ref, err)
	if !strings.Contains(h, "guest-"+image.GuestVersion) || !strings.Contains(h, "--image official") {
		t.Errorf("prebaked hint = %q", h)
	}
	h = fetchHint("dev", ref, errors.New("connection reset"))
	if !strings.Contains(h, "re-run") {
		t.Errorf("generic hint = %q", h)
	}
	h = fetchHint("dev", machine.ImageRef{Source: "byo", Release: "./x.raw"}, image.ErrNoChecksum)
	if strings.Contains(h, "--image official") {
		t.Errorf("byo hint = %q", h)
	}
}

func TestStageTimeoutScalesUnderTCG(t *testing.T) {
	t.Setenv(qemu.AccelEnv, "hvf")
	if got := stageTimeout(provisionTimeout); got != provisionTimeout {
		t.Errorf("hvf: %v", got)
	}
	t.Setenv(qemu.AccelEnv, qemu.AccelTCG)
	if got := stageTimeout(provisionTimeout); got != tcgTimeoutScale*provisionTimeout {
		t.Errorf("tcg: %v", got)
	}
	if stageTimeout(sshTimeout) <= 30*time.Minute {
		t.Errorf("ssh under tcg = %v", stageTimeout(sshTimeout))
	}
}
