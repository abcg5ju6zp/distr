package controllerapply

import (
	"errors"
	"io/fs"
	"os"

	"github.com/google/uuid"
	"github.com/gofrs/flock"
)

// Unlock releases a deployment lock acquired with TryLock.
type Unlock func()

// TryLock takes the advisory lock of one deployment without waiting. A false result means another
// process or goroutine, including a controller replacing itself through a self-update, is currently
// applying that deployment, and the caller must skip it for this tick. The lock is bound to the file
// descriptor, so a killed process releases it automatically.
func (s *Store) TryLock(deploymentID uuid.UUID) (Unlock, bool, error) {
	lockDir := s.locksDir()
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, false, err
	}
	fl := flock.New(s.lockFile(deploymentID))
	locked, err := fl.TryLock()
	if err != nil || !locked {
		return nil, locked, err
	}
	return func() { _ = fl.Unlock() }, true, nil
}

// RemoveLockFile deletes the lock file of a fully uninstalled deployment. It must only be called by
// the lock holder.
func (s *Store) RemoveLockFile(deploymentID uuid.UUID) error {
	if err := os.Remove(s.lockFile(deploymentID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
