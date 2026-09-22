// Package atomicfile replaces file contents with a single rename, so a
// concurrent reader never observes a partially written result and an
// interrupted write cannot corrupt the previous contents.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces path with data through a temporary file in the same
// directory, followed by fsync and rename. If path already exists and is a
// regular file, the replacement keeps its current permission bits; perm
// applies only when path does not yet exist. Write refuses to replace a
// directory, symlink, or other non-regular file.
func Write(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	mode := perm
	switch info, err := os.Lstat(path); {
	case err == nil:
		if !info.Mode().IsRegular() {
			return fmt.Errorf("atomicfile: %s: refusing to replace a non-regular file", path)
		}
		mode = info.Mode().Perm()
	case errors.Is(err, fs.ErrNotExist):
		// Creating a new file; use the caller-supplied permission.
	default:
		return fmt.Errorf("atomicfile: stat %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(dir, ".msm-tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temporary file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("atomicfile: sync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close %s: %w", tmpPath, err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("atomicfile: chmod %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomicfile: rename %s to %s: %w", tmpPath, path, err)
	}
	committed = true

	// Fsync the directory entry too, so the rename itself survives a crash
	// on filesystems that require it (an unsupported Sync is not fatal).
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		dirHandle.Close()
	}
	return nil
}
