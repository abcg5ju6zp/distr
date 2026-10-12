package controllerapply

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// WriteFileAtomic replaces path with data without readers ever seeing a partial file: the content is
// written and fsynced in a temp file in the same directory, renamed into place and the directory is
// fsynced so the replacement survives a power loss. A failure leaves the previous file untouched.
func WriteFileAtomic(fpath string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(fpath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(fpath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, fpath); err != nil {
		return err
	}
	committed = true
	return syncDir(dir)
}

func writeJSONAtomic(fpath string, value any) error {
	data, err := json.MarshalIndent(value, "", "\t")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(fpath, data, 0o600)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// isTempName reports whether name is unpublished staging content left by an interrupted write. Temp
// files always start with a dot, which published state and project files never do.
func isTempName(name string) bool {
	return len(name) > 0 && name[0] == '.'
}

// StagingDir creates and returns an empty staging directory next to the deployment project directories.
// Project files are fully written and validated there before being committed with CommitProjectFile.
func (s *Store) StagingDir(deploymentID uuid.UUID) (string, error) {
	if err := s.ensureDir(s.projectsDir()); err != nil {
		return "", err
	}
	return os.MkdirTemp(s.projectsDir(), ".staging-"+deploymentID.String()+"-*")
}

func RemoveStagingDir(dir string) error {
	return os.RemoveAll(dir)
}

// WriteStagedFile writes name into a staging directory and fsyncs it, but publishes nothing.
func WriteStagedFile(stagingDir, name string, data []byte) error {
	return WriteFileAtomic(filepath.Join(stagingDir, name), data, 0o600)
}

// CommitProjectFile atomically moves a staged file into the deployment's project directory. It is a
// same-filesystem rename, so the published file is either the old or the new one, never a mix.
func (s *Store) CommitProjectFile(deploymentID uuid.UUID, stagingDir, name string) error {
	targetDir := s.ProjectDir(deploymentID)
	if err := s.ensureDir(targetDir); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(stagingDir, name), filepath.Join(targetDir, name)); err != nil {
		return err
	}
	return syncDir(targetDir)
}

// RemoveProjectFile retires a file the new revision no longer declares by swapping it into the staging
// directory, which is deleted afterwards. A crash therefore never leaves a half-removed file next to
// the new content.
func (s *Store) RemoveProjectFile(deploymentID uuid.UUID, stagingDir, name string) error {
	target := filepath.Join(s.ProjectDir(deploymentID), name)
	if err := os.Rename(target, filepath.Join(stagingDir, name)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return syncDir(s.ProjectDir(deploymentID))
}
