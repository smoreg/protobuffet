//go:build unix

package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an exclusive lock on a data directory, held for the lifetime of the
// process that owns it.
type Lock struct{ f *os.File }

// LockStore takes an exclusive advisory lock on the data directory.
//
// The store is single-writer by design, but nothing enforced it across
// processes: a `buffetd -gc` run against a live registry's volume, which the
// docs used to suggest, could interleave its index rewrite with a publish and
// lose one of them. The lock makes that combination fail loudly instead.
func LockStore(root string) (*Lock, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, ".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	// A file descriptor from the Go runtime always fits in an int; the
	// uintptr in the API is a historical artefact.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		f.Close() //nolint:errcheck // already failing
		return nil, fmt.Errorf("data directory %s is locked by another process (a running buffetd, or another gc): %w", root, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil { //nolint:gosec // fd fits in int
		l.f.Close() //nolint:errcheck // already failing
		return err
	}
	return l.f.Close()
}
