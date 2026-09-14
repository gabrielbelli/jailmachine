package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gabrielbelli/jailmachine/internal/backend"
	"github.com/gabrielbelli/jailmachine/internal/machine"
)

// JournalVersion is the suspend journal schema version.
const JournalVersion = 1

// Files of a suspend besides the journal and image named in package machine.
const (
	// SuspendTmpFile is the image while QEMU writes it; it is renamed to
	// machine.SuspendImageFile only once it is durable.
	SuspendTmpFile = machine.SuspendImageFile + ".tmp"
	// BadJournalFile is where Repair keeps an unparseable journal.
	BadJournalFile = machine.SuspendJournalFile + ".bad"
)

// errBadJournal is wrapped by readJournal when the file exists but is not a
// journal this version understands.
var errBadJournal = errors.New("qemu: unreadable suspend journal")

// Journal is suspend.json: the record of a suspend in progress ("saving") or
// complete ("saved"). State is computed from it plus the image size and the
// pid file (ADR 0009).
type Journal struct {
	Version     int                  `json:"version"`
	Phase       backend.SuspendPhase `json:"phase"`
	Reason      string               `json:"reason,omitempty"`
	OwnerPID    int                  `json:"owner_pid,omitempty"`
	StartedAt   time.Time            `json:"started_at,omitzero"`
	SavedAt     time.Time            `json:"saved_at,omitzero"`
	Argv        []string             `json:"argv"`
	MachineType string               `json:"machine_type"`
	QEMUVersion string               `json:"qemu_version,omitempty"`
	QEMUBinary  string               `json:"qemu_binary"`
	Hardware    Hardware             `json:"hardware"`
	// ImageBytes is the logical size of the committed image; a "saved"
	// journal is valid only while the image still has exactly this size.
	ImageBytes     int64             `json:"image_bytes,omitempty"`
	ImageAllocated int64             `json:"image_allocated,omitempty"`
	Fingerprints   *Fingerprints     `json:"fingerprints,omitempty"`
	Meta           map[string]string `json:"meta,omitempty"`
	Stats          *SuspendStats     `json:"stats,omitempty"`
}

// Hardware is the part of the machine record the saved state depends on. A
// record that no longer matches cannot be restored.
type Hardware struct {
	CPUs      int      `json:"cpus"`
	MemoryMiB int      `json:"memory_mib"`
	MAC       string   `json:"mac"`
	SSHPort   int      `json:"ssh_port"`
	ShareTags []string `json:"share_tags"`
}

// hardwareOf returns the saved-state-relevant part of a record.
func hardwareOf(m *machine.Machine) Hardware {
	tags := []string{}
	for _, s := range m.Shares {
		tags = append(tags, s.Tag)
	}
	return Hardware{CPUs: m.CPUs, MemoryMiB: m.MemoryMiB, MAC: m.MAC, SSHPort: m.SSHPort, ShareTags: tags}
}

// diff describes how h differs from want, or "" when they are equal.
func (h Hardware) diff(want Hardware) string {
	switch {
	case h.CPUs != want.CPUs:
		return fmt.Sprintf("cpus %d, saved with %d", h.CPUs, want.CPUs)
	case h.MemoryMiB != want.MemoryMiB:
		return fmt.Sprintf("memory %d MiB, saved with %d MiB", h.MemoryMiB, want.MemoryMiB)
	case h.MAC != want.MAC:
		return fmt.Sprintf("MAC %s, saved with %s", h.MAC, want.MAC)
	case h.SSHPort != want.SSHPort:
		return fmt.Sprintf("ssh port %d, saved with %d", h.SSHPort, want.SSHPort)
	}
	if len(h.ShareTags) != len(want.ShareTags) {
		return fmt.Sprintf("%d shares, saved with %d", len(h.ShareTags), len(want.ShareTags))
	}
	for i := range h.ShareTags {
		if h.ShareTags[i] != want.ShareTags[i] {
			return fmt.Sprintf("share %s, saved with %s", h.ShareTags[i], want.ShareTags[i])
		}
	}
	return ""
}

// Fingerprints identify the disk and firmware variable store a saved state
// was taken against.
type Fingerprints struct {
	Disk    Fingerprint `json:"disk"`
	EFIVars Fingerprint `json:"efivars"`
}

// Fingerprint is a file's size and modification time.
type Fingerprint struct {
	Size    int64 `json:"size"`
	MtimeNS int64 `json:"mtime_ns"`
}

// SuspendStats are the migration's own figures.
type SuspendStats struct {
	TotalMS     int64 `json:"total_ms"`
	DowntimeMS  int64 `json:"downtime_ms"`
	Transferred int64 `json:"transferred"`
}

// journalPaths are the suspend files of a machine directory.
type journalPaths struct {
	Journal, Image, Tmp, Bad string
}

func suspendPaths(dir string) journalPaths {
	return journalPaths{
		Journal: filepath.Join(dir, machine.SuspendJournalFile),
		Image:   filepath.Join(dir, machine.SuspendImageFile),
		Tmp:     filepath.Join(dir, SuspendTmpFile),
		Bad:     filepath.Join(dir, BadJournalFile),
	}
}

// readJournal reads a journal. A missing file is returned as os.ErrNotExist
// (wrapped); a file that was read but is not a version-1 journal in a known
// phase wraps errBadJournal. Any other error (EIO, EACCES, EMFILE) is
// returned as it is: it says nothing about the journal, so no caller may set
// the journal or its image aside because of it.
func readJournal(path string) (*Journal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%w %s: %v", errBadJournal, path, err)
	}
	if j.Version != JournalVersion {
		return nil, fmt.Errorf("%w %s: version %d", errBadJournal, path, j.Version)
	}
	if j.Phase != backend.SuspendSaving && j.Phase != backend.SuspendSaved {
		return nil, fmt.Errorf("%w %s: phase %q", errBadJournal, path, j.Phase)
	}
	return &j, nil
}

// writeJournal replaces the journal atomically and durably: a temporary
// sibling is written and synced (F_FULLFSYNC on darwin), renamed into place,
// and the directory is synced so that the rename itself survives a crash.
func writeJournal(path string, j *Journal) error {
	j.Version = JournalVersion
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("qemu: encoding suspend journal: %w", err)
	}
	if err := writeAtomic(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a directory's entries (a rename, an unlink) durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("qemu: syncing %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("qemu: syncing %s: %w", dir, err)
	}
	return nil
}

// validImage reports whether a "saved" journal's image is present with the
// size the journal committed. An image that cannot be examined is not valid;
// callers that would discard an invalid image use checkImage instead.
func validImage(image string, j *Journal) bool {
	ok, _ := checkImage(image, j)
	return ok
}

// checkImage is validImage that tells "invalid" (missing, or not the
// committed size) apart from "cannot tell": a stat that failed for any reason
// but a missing file is returned as an error, and must not lead to a discard.
func checkImage(image string, j *Journal) (bool, error) {
	if j == nil || j.Phase != backend.SuspendSaved || j.ImageBytes <= 0 {
		return false, nil
	}
	st, err := os.Stat(image)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("qemu: examining the saved image: %w", err)
	}
	return st.Mode().IsRegular() && st.Size() == j.ImageBytes, nil
}

// fingerprint returns a file's size and modification time.
func fingerprint(path string) (Fingerprint, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Fingerprint{}, err
	}
	return Fingerprint{Size: st.Size(), MtimeNS: st.ModTime().UnixNano()}, nil
}

// fingerprintsOf fingerprints a machine's disk and firmware variable store.
func fingerprintsOf(dir string) (*Fingerprints, error) {
	disk, err := fingerprint(filepath.Join(dir, machine.DiskFile))
	if err != nil {
		return nil, err
	}
	vars, err := fingerprint(filepath.Join(dir, machine.EFIVarsFile))
	if err != nil {
		return nil, err
	}
	return &Fingerprints{Disk: disk, EFIVars: vars}, nil
}

// allocatedBytes is the space a file takes on disk (st_blocks * 512).
func allocatedBytes(st os.FileInfo) int64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return int64(s.Blocks) * 512
	}
	return st.Size()
}

// removeJournal deletes the journal and syncs the directory, so that once it
// returns nil a crash cannot bring the journal back.
func removeJournal(sp journalPaths) error {
	if err := os.Remove(sp.Journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("qemu: removing %s: %w", sp.Journal, err)
	}
	return syncDir(filepath.Dir(sp.Journal))
}

// discardJournalThenImage removes the journal first, durably, and only then
// the image and any partial image. A failure half way therefore never leaves
// a journal that points at a missing or partial image.
func discardJournalThenImage(sp journalPaths) error {
	if err := removeJournal(sp); err != nil {
		return err
	}
	return removeImages(sp)
}

// removeImages deletes the image and the partial image.
func removeImages(sp journalPaths) error {
	return removeAll(sp.Image, sp.Tmp)
}
