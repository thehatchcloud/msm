package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// A hard-link snapshot is the Go form of the legacy
//
//	rsync -aH --link-dest=<dir>/latest <world> <dir>/<timestamp>
//
// Each snapshot is a complete, browsable copy of the world in
// <dir>/<timestamp>/<world>/..., and <dir>/latest names the newest one. A
// file that has not changed since the snapshot "latest" names is a hard link
// to that snapshot's file, so it takes no further space; a file that has
// changed is written to a new file. Nothing in an earlier snapshot is ever
// written to: a linked file shares its inode with the earlier snapshot, so it
// is never chmod-ed, touched or opened for writing here, and a changed file is
// a new inode, never an edit in place.
//
// "Unchanged" is rsync's quick check, made stricter: the same size, the same
// permission bits and the same modification time to the nanosecond. On a
// filesystem that keeps only whole seconds the copies do not compare equal,
// and the files are copied instead of linked, which costs space but is never
// wrong.
const (
	latestName = "latest"
	// snapshotTmpKind marks the directory a snapshot is built in.
	snapshotTmpKind = "snapshot-"
)

// snapshotStats is what a snapshot did.
type snapshotStats struct {
	Path   string
	Linked int
	Copied int
	Links  int
	Dirs   int
	Bytes  int64
	// NoLinks is why files were copied that could have been linked, when the
	// filesystem refused hard links.
	NoLinks string
}

// writeSnapshot publishes a snapshot of entries (top-level name top) in dir
// and points dir/latest at it. Like writeArchive it builds under a temporary
// name, checks the result, and publishes with a rename that never replaces an
// existing snapshot; on any failure nothing is published and "latest" is
// left alone.
func writeSnapshot(ctx context.Context, ops fsOps, dir, top string, when time.Time, entries []entry) (stats snapshotStats, err error) {
	prev, warn := previousSnapshot(dir, top)
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return stats, err
	}
	tmp := filepath.Join(dir, tmpPrefix+snapshotTmpKind+hex.EncodeToString(b[:])+tmpSuffix)
	if err := os.Mkdir(tmp, archiveDirPm); err != nil {
		return stats, fmt.Errorf("create snapshot in %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			removeTree(tmp)
		}
	}()
	s := &snapshotWriter{ctx: ctx, ops: ops, root: tmp, prev: prev, stats: &stats, prevDirs: map[string]bool{}}
	if warn != "" {
		stats.NoLinks = warn
	}
	if err := s.build(entries); err != nil {
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		return stats, context.Cause(ctx)
	}
	if err := verifySnapshot(tmp, entries, s.sizes); err != nil {
		return stats, err
	}
	syncDir(tmp)
	final, err := publishDir(tmp, dir, when)
	if err != nil {
		return stats, err
	}
	stats.Path = final
	syncDir(dir)
	if err := pointLatest(dir, filepath.Base(final)); err != nil {
		return stats, fmt.Errorf("snapshot %s was published, but %s was not updated: %w", final, latestName, err)
	}
	return stats, nil
}

type snapshotWriter struct {
	ctx   context.Context
	ops   fsOps
	root  string
	prev  string // the previous snapshot's copy of the world; "" for none
	stats *snapshotStats
	// prevDirs records which directories exist in the previous snapshot as
	// real directories, so a link is only made through a path of them.
	prevDirs map[string]bool
	noLink   bool
	sizes    map[string]int64
	dirs     []entry
}

func (s *snapshotWriter) build(entries []entry) error {
	s.sizes = map[string]int64{}
	for _, e := range entries {
		if err := s.ctx.Err(); err != nil {
			return context.Cause(s.ctx)
		}
		dst := filepath.Join(s.root, filepath.FromSlash(e.Name))
		switch e.Kind {
		case kindDir:
			if err := s.dir(e, dst); err != nil {
				return err
			}
		case kindLink:
			if err := os.Symlink(e.Target, dst); err != nil {
				return fmt.Errorf("snapshot %s: %w", e.Src, err)
			}
			// Best effort: the time of a link itself.
			ts := []unix.Timespec{unix.NsecToTimespec(e.Info.ModTime().UnixNano()), unix.NsecToTimespec(e.Info.ModTime().UnixNano())}
			_ = unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW)
			s.stats.Links++
		default:
			if err := s.file(e, dst); err != nil {
				return err
			}
		}
	}
	// Directories last, deepest first: their permissions and times can only
	// be set once nothing more is created in them.
	for i := len(s.dirs) - 1; i >= 0; i-- {
		d := s.dirs[i]
		dst := filepath.Join(s.root, filepath.FromSlash(d.Name))
		if err := os.Chmod(dst, d.Info.Mode().Perm()); err != nil {
			return fmt.Errorf("snapshot %s: %w", d.Src, err)
		}
		if err := os.Chtimes(dst, d.Info.ModTime(), d.Info.ModTime()); err != nil {
			return fmt.Errorf("snapshot %s: %w", d.Src, err)
		}
	}
	return nil
}

func (s *snapshotWriter) dir(e entry, dst string) error {
	if err := os.Mkdir(dst, archiveDirPm); err != nil {
		return fmt.Errorf("snapshot %s: %w", e.Src, err)
	}
	s.dirs = append(s.dirs, e)
	s.stats.Dirs++
	if s.prev != "" {
		rel := relInside(e.Name)
		if rel == "" {
			s.prevDirs[e.Name] = isRealDir(s.prev)
		} else if s.prevDirs[parentName(e.Name)] {
			s.prevDirs[e.Name] = isRealDir(filepath.Join(s.prev, filepath.FromSlash(rel)))
		}
	}
	return nil
}

// relInside is the path of an entry name below the top-level directory.
func relInside(name string) string {
	name = trimSlash(name)
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			return name[i+1:]
		}
	}
	return ""
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func parentName(name string) string {
	name = trimSlash(name)
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' {
			return name[:i+1]
		}
	}
	return ""
}

func isRealDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

func (s *snapshotWriter) file(e entry, dst string) error {
	in, err := s.ops.Open(e.Src)
	if err != nil {
		return fmt.Errorf("read %s: %w", e.Src, err)
	}
	defer in.Close()
	size, mtime := e.Info.Size(), e.Info.ModTime()
	if st, ok := in.(interface{ Stat() (fs.FileInfo, error) }); ok {
		if fi, err := st.Stat(); err == nil {
			size, mtime = fi.Size(), fi.ModTime()
		}
	}
	perm := e.Info.Mode().Perm()
	if old := s.previousFile(e, size, mtime, perm); old != "" && !s.noLink {
		err := s.ops.Link(old, dst)
		if err == nil {
			s.stats.Linked++
			s.sizes[e.Name] = size
			return nil
		}
		switch {
		case errors.Is(err, unix.EXDEV), errors.Is(err, unix.EPERM), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
			s.noLink = true
			s.stats.NoLinks = fmt.Sprintf("hard links are not available between %s and the previous snapshot (%v); files are copied", s.root, err)
		case errors.Is(err, unix.EMLINK):
			// This one file has too many links; later files may be fine.
		default:
			return fmt.Errorf("link %s: %w", e.Src, err)
		}
	}
	return s.copy(in, e, dst, size, mtime, perm)
}

// previousFile returns the previous snapshot's copy of e when it is a plain
// file that a link to would be indistinguishable from a fresh copy.
func (s *snapshotWriter) previousFile(e entry, size int64, mtime time.Time, perm fs.FileMode) string {
	if s.prev == "" || !s.prevDirs[parentName(e.Name)] {
		return ""
	}
	old := filepath.Join(s.prev, filepath.FromSlash(relInside(e.Name)))
	info, err := os.Lstat(old)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	if info.Size() != size || info.Mode().Perm() != perm || !info.ModTime().Equal(mtime) {
		return ""
	}
	return old
}

func (s *snapshotWriter) copy(in io.Reader, e entry, dst string, size int64, mtime time.Time, perm fs.FileMode) (err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, archiveFilePm)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", e.Src, err)
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("snapshot %s: %w", e.Src, cerr)
		}
	}()
	n, err := io.Copy(out, &ctxReader{ctx: s.ctx, r: in})
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", e.Src, err)
	}
	if n != size {
		return fmt.Errorf("snapshot %s: it changed while it was copied (%d bytes read, %d expected)", e.Src, n, size)
	}
	if err := out.Chmod(perm); err != nil {
		return fmt.Errorf("snapshot %s: %w", e.Src, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("snapshot %s: %w", e.Src, err)
	}
	if err := os.Chtimes(dst, mtime, mtime); err != nil {
		return fmt.Errorf("snapshot %s: %w", e.Src, err)
	}
	s.stats.Copied++
	s.stats.Bytes += n
	s.sizes[e.Name] = n
	return nil
}

// previousSnapshot returns the previous snapshot's copy of the world, found
// through the "latest" link, or "" with a reason when there is none to use.
func previousSnapshot(dir, top string) (prev, why string) {
	link := filepath.Join(dir, latestName)
	info, err := os.Lstat(link)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ""
	}
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return "", fmt.Sprintf("%s is not a link to a snapshot; this snapshot copies every file", link)
	}
	target, err := os.Readlink(link)
	if err != nil || target == "" || target != filepath.Base(target) || target[0] == '.' {
		return "", fmt.Sprintf("%s does not name a snapshot in %s; this snapshot copies every file", link, dir)
	}
	root := filepath.Join(dir, target, top)
	if !isRealDir(filepath.Join(dir, target)) || !isRealDir(root) {
		return "", fmt.Sprintf("%s names %s, which is not a snapshot of %s; this snapshot copies every file", link, target, top)
	}
	return root, ""
}

// verifySnapshot checks that the temporary snapshot holds every planned
// entry, with the right kind and size, and nothing else.
func verifySnapshot(root string, entries []entry, sizes map[string]int64) error {
	want := map[string]entry{}
	for _, e := range entries {
		want[trimSlash(e.Name)] = e
	}
	seen := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		e, ok := want[filepath.ToSlash(rel)]
		if !ok {
			return fmt.Errorf("verify snapshot: unexpected %s", rel)
		}
		seen++
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch e.Kind {
		case kindDir:
			if !info.IsDir() {
				return fmt.Errorf("verify snapshot: %s is not a directory", rel)
			}
		case kindLink:
			if info.Mode()&fs.ModeSymlink == 0 {
				return fmt.Errorf("verify snapshot: %s is not a link", rel)
			}
		default:
			if !info.Mode().IsRegular() || info.Size() != sizes[e.Name] {
				return fmt.Errorf("verify snapshot: %s is not the file that was copied", rel)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(want) {
		return fmt.Errorf("verify snapshot: it holds %d entries, expected %d", seen, len(want))
	}
	return nil
}

// publishDir renames the finished snapshot tmp to the first free
// timestamped name in dir. A rename that cannot replace, unlike a plain one,
// never overwrites a snapshot that is already there.
func publishDir(tmp, dir string, when time.Time) (string, error) {
	base := when.Format(archiveStamp)
	for n := 0; n < 1000; n++ {
		name := base
		if n > 0 {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		final := filepath.Join(dir, name)
		ok, err := renameNoReplace(tmp, final)
		if !ok {
			// The filesystem has no exclusive rename. This runs under the
			// server's lock, so only a stranger could race the check.
			if _, serr := os.Lstat(final); serr == nil {
				continue
			}
			err = os.Rename(tmp, final)
		}
		if errors.Is(err, fs.ErrExist) || errors.Is(err, unix.ENOTEMPTY) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("publish snapshot %s: %w", final, err)
		}
		return final, nil
	}
	return "", fmt.Errorf("publish snapshot in %s: every name for %s is taken", dir, base)
}

// pointLatest makes dir/latest a relative link to the snapshot name,
// replacing the old link in one step, so there is no moment without one.
func pointLatest(dir, name string) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, tmpPrefix+hex.EncodeToString(b[:])+tmpSuffix)
	if err := os.Symlink(name, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, latestName)); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// removeTree removes a directory this package built, including one whose
// directories were given read-only permissions to match the world's.
func removeTree(path string) error {
	makeRemovable(path)
	return os.RemoveAll(path)
}

// makeRemovable gives every directory below path owner access, so that a
// tree that mirrors a world with read-only directories can be removed.
func makeRemovable(path string) {
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if d != nil && d.IsDir() {
			// The walk calls this before it reads the directory, so a
			// directory with no permissions can still be opened.
			os.Chmod(p, 0o700)
		}
		return nil
	})
}
