package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"

	"github.com/gofrs/flock"
)

// ErrNotFound is returned when a machine directory or record does not exist.
var ErrNotFound = errors.New("machine not found")

// ErrLocked is returned by Lock when another process holds the lock.
var ErrLocked = errors.New("machine is locked by another process")

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ValidateName rejects names that could escape the machines directory.
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	return nil
}

// Store is a directory of machines rooted at <Root>/machines/.
type Store struct {
	Root string
}

// NewStore returns a Store rooted at stateRoot (e.g. ~/.jailmachine).
func NewStore(stateRoot string) *Store { return &Store{Root: stateRoot} }

// Dir returns <root>/machines/<name>.
func (s *Store) Dir(name string) string {
	return filepath.Join(s.Root, MachinesDir, name)
}

// Path returns the absolute path of a fixed file inside the machine directory.
func (s *Store) Path(name, file string) string {
	return filepath.Join(s.Dir(name), file)
}

// Load reads machine.json for name.
func (s *Store) Load(name string) (*Machine, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.Path(name, RecordFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, err
	}
	var m Machine
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.Path(name, RecordFile), err)
	}
	if m.Version > Version {
		return nil, fmt.Errorf("%s: record version %d is newer than supported %d", name, m.Version, Version)
	}
	if m.Name == "" {
		m.Name = name
	}
	m.Dir = s.Dir(name)
	return &m, nil
}

// Save writes machine.json atomically (temp file + rename), creating the
// machine directory if needed.
func (s *Store) Save(m *Machine) error {
	if err := ValidateName(m.Name); err != nil {
		return err
	}
	if m.Version == 0 {
		m.Version = Version
	}
	dir := s.Dir(m.Name)
	m.Dir = dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, RecordFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, RecordFile)); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so that a rename into it survives a crash.
// Platforms that refuse to fsync directories are not an error.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EBADF) {
		return err
	}
	return nil
}

// List returns every machine with a readable record, sorted by name.
// Directories without machine.json are skipped.
func (s *Store) List() ([]*Machine, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, MachinesDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Machine
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := s.Load(e.Name())
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Exists reports whether a record exists for name.
func (s *Store) Exists(name string) bool {
	_, err := os.Stat(s.Path(name, RecordFile))
	return err == nil
}

// Delete removes the whole machine directory. Deleting a machine that does
// not exist is not an error (rm always converges to "gone", ADR 0005).
func (s *Store) Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.Dir(name))
}

// Lock takes the per-machine advisory lock, creating the directory if
// needed. It fails immediately with ErrLocked if another process holds it.
// The returned func releases the lock.
func (s *Store) Lock(name string) (unlock func(), err error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir(name), 0o700); err != nil {
		return nil, err
	}
	fl := flock.New(s.Path(name, LockFile))
	ok, err := fl.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrLocked, name)
	}
	return func() { _ = fl.Unlock() }, nil
}
