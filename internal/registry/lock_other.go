//go:build !unix

package registry

import "fmt"

// Lock is the non-Unix stand-in for the data directory lock.
type Lock struct{}

// LockStore refuses to run the server where the store's guarantees do not
// hold. The CLI imports this package and is built for Windows, so the symbol
// has to exist; the server is not supported there, because the store relies
// on flock, on fsyncing a directory and on renaming over an existing file.
func LockStore(root string) (*Lock, error) {
	return nil, fmt.Errorf("the registry server is only supported on Unix: locking %s needs flock", root)
}

// Release is a no-op.
func (l *Lock) Release() error { return nil }
