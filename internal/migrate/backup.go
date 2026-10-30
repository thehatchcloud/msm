package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thehatchcloud/msm/internal/atomicfile"
)

// BackupFile is one legacy control file as it was before the migration.
type BackupFile struct {
	// Path is the logical path of the file.
	Path string `json:"path"`
	// Kind is "file", "symlink" or "absent". An absent file is recorded so
	// rollback can tell "was not there" from "was not backed up".
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode,omitempty"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	SHA256 string `json:"sha256,omitempty"`
	Link   string `json:"link,omitempty"`
	// Stored is the copy's name below the backup directory.
	Stored string `json:"stored,omitempty"`
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (l Layout) backupDir() string { return filepath.Join(l.P(l.StateDir), "backup") }
func (l Layout) savedDir() string  { return filepath.Join(l.P(l.StateDir), "saved") }

// controlFiles are the legacy files the migration can change or that the
// legacy manager reads, in the order they are backed up. The configuration
// is read by both managers and migration does not change it; it is backed up
// all the same, so rollback can prove it came back unchanged.
func (l Layout) controlFiles() []string {
	return []string{l.Conf, l.Cron, l.Init, l.Profile, l.Completion, l.LegacyUnit, l.Command}
}

// backupControl copies every control file into the state directory and
// returns what was recorded. It refuses to run twice into the same
// directory, so an earlier backup is never overwritten.
func backupControl(l Layout) ([]BackupFile, error) {
	dir := l.backupDir()
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("migrate: %s already exists; a previous migration left its backup there, so move it away or roll back first", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("migrate: create backup directory: %w", err)
	}
	var out []BackupFile
	for i, logical := range l.controlFiles() {
		phys := l.P(logical)
		info, err := os.Lstat(phys)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, BackupFile{Path: logical, Kind: "absent"})
			continue
		case err != nil:
			return nil, fmt.Errorf("migrate: back up %s: %w", logical, err)
		}
		bf := BackupFile{Path: logical, Mode: uint32(info.Mode().Perm())}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			bf.UID, bf.GID = int(st.Uid), int(st.Gid)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			bf.Kind = "symlink"
			if bf.Link, err = os.Readlink(phys); err != nil {
				return nil, err
			}
		case info.Mode().IsRegular():
			bf.Kind = "file"
			data, err := os.ReadFile(phys)
			if err != nil {
				return nil, fmt.Errorf("migrate: back up %s: %w", logical, err)
			}
			bf.SHA256 = hashBytes(data)
			bf.Stored = fmt.Sprintf("%02d-%s", i, filepath.Base(logical))
			if err := os.WriteFile(filepath.Join(dir, bf.Stored), data, 0o600); err != nil {
				return nil, fmt.Errorf("migrate: back up %s: %w", logical, err)
			}
		default:
			return nil, fmt.Errorf("migrate: %s is neither a regular file nor a link; refusing to migrate around it", logical)
		}
		out = append(out, bf)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(filepath.Join(dir, "manifest.json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	return out, nil
}

// verifyBackup rereads every stored copy and checks it against the manifest,
// so a backup that cannot restore a file is found before anything changes.
func verifyBackup(l Layout, files []BackupFile) error {
	for _, bf := range files {
		if bf.Kind != "file" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(l.backupDir(), bf.Stored))
		if err != nil {
			return fmt.Errorf("migrate: backup of %s is unreadable: %w", bf.Path, err)
		}
		if hashBytes(data) != bf.SHA256 {
			return fmt.Errorf("migrate: backup of %s does not match what was read from it", bf.Path)
		}
	}
	return nil
}

func findBackup(files []BackupFile, logical string) (BackupFile, bool) {
	for _, bf := range files {
		if bf.Path == logical {
			return bf, true
		}
	}
	return BackupFile{}, false
}

// restore puts a backed-up file or link back, replacing whatever is at the
// path now. What it replaces is first copied into the saved directory, so
// nothing is destroyed.
func restore(l Layout, bf BackupFile, euid int) error {
	phys := l.P(bf.Path)
	if err := saveAside(l, bf.Path); err != nil {
		return err
	}
	switch bf.Kind {
	case "absent":
		if err := os.Remove(phys); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	case "symlink":
		if err := os.Remove(phys); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Symlink(bf.Link, phys); err != nil {
			return err
		}
		if euid == 0 {
			return os.Lchown(phys, bf.UID, bf.GID)
		}
		return nil
	}
	data, err := os.ReadFile(filepath.Join(l.backupDir(), bf.Stored))
	if err != nil {
		return fmt.Errorf("migrate: read the backup of %s: %w", bf.Path, err)
	}
	if hashBytes(data) != bf.SHA256 {
		return fmt.Errorf("migrate: the backup of %s is damaged (checksum differs from the manifest)", bf.Path)
	}
	if info, err := os.Lstat(phys); err == nil && !info.Mode().IsRegular() {
		if err := os.Remove(phys); err != nil {
			return err
		}
	}
	if err := atomicfile.Write(phys, data, fs.FileMode(bf.Mode)); err != nil {
		return err
	}
	if err := os.Chmod(phys, fs.FileMode(bf.Mode)); err != nil {
		return err
	}
	if euid == 0 {
		return os.Lchown(phys, bf.UID, bf.GID)
	}
	return nil
}

// saveAside copies the current regular file at logical, if there is one,
// into the saved directory before something replaces or removes it.
func saveAside(l Layout, logical string) error {
	phys := l.P(logical)
	info, err := os.Lstat(phys)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(phys)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.savedDir(), 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s", filepath.Base(logical), hashBytes(data)[:12])
	return os.WriteFile(filepath.Join(l.savedDir(), name), data, 0o600)
}
