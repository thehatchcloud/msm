package worlds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
)

// ramManifestName is the file in a RAM copy that records what the copy and
// the disk copy looked like when they last matched. It is never copied to
// disk and never part of a fingerprint.
const ramManifestName = ".msm-ram.json"

// copyOptions says how copyWorld copies a tree.
type copyOptions struct {
	// Skip names top-level entries that are not copied (the RAM flag file
	// and the manifest).
	Skip map[string]bool
	// Owner, when set and the process is root, receives every file, link and
	// directory created, so a server running as that user can write them.
	Owner *identity.Identity
}

// copyWorld copies the directory tree src to the new path dst. Every step
// goes through descriptors opened without following symbolic links, so a
// tree whose owner can rename things while a privileged copy runs cannot
// redirect the copy to another part of the system. Symbolic links are copied
// as links; anything but files, directories and links is refused. Files are
// flushed before the copy is reported complete, and modification times are
// kept so that an unchanged file looks unchanged in a fingerprint.
func copyWorld(ctx context.Context, src, dst string, opt copyOptions) error {
	srcFD, err := unix.Open(src, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: src, Err: err}
	}
	defer unix.Close(srcFD)
	var st unix.Stat_t
	if err := unix.Fstat(srcFD, &st); err != nil {
		return &fs.PathError{Op: "stat", Path: src, Err: err}
	}
	if err := unix.Mkdir(dst, 0o700); err != nil {
		return &fs.PathError{Op: "mkdir", Path: dst, Err: err}
	}
	dstFD, err := unix.Open(dst, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: dst, Err: err}
	}
	defer unix.Close(dstFD)
	c := &copier{ctx: ctx, opt: opt, root: src}
	if err := c.dir(srcFD, dstFD, "", &st); err != nil {
		return err
	}
	if o := opt.Owner; o != nil && os.Geteuid() == 0 {
		if err := unix.Fchown(dstFD, o.UID, o.GID); err != nil {
			return &fs.PathError{Op: "chown", Path: dst, Err: err}
		}
	}
	if err := unix.Fchmod(dstFD, uint32(st.Mode&0o7777)); err != nil {
		return &fs.PathError{Op: "chmod", Path: dst, Err: err}
	}
	return nil
}

type copier struct {
	ctx  context.Context
	opt  copyOptions
	root string
}

func (c *copier) chown(fd int, name string) error {
	o := c.opt.Owner
	if o == nil || os.Geteuid() != 0 {
		return nil
	}
	return unix.Fchownat(fd, name, o.UID, o.GID, unix.AT_SYMLINK_NOFOLLOW)
}

func (c *copier) dir(srcFD, dstFD int, rel string, st *unix.Stat_t) error {
	dup, err := unix.Dup(srcFD)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), c.root)
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return &fs.PathError{Op: "read", Path: filepath.Join(c.root, rel), Err: err}
	}
	for _, e := range entries {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if rel == "" && c.opt.Skip[name] {
			continue
		}
		path := filepath.Join(c.root, rel, name)
		var es unix.Stat_t
		if err := unix.Fstatat(srcFD, name, &es, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return &fs.PathError{Op: "stat", Path: path, Err: err}
		}
		switch es.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := c.subdir(srcFD, dstFD, rel, name, &es); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := c.file(srcFD, dstFD, path, name, &es); err != nil {
				return err
			}
		case unix.S_IFLNK:
			buf := make([]byte, 4096)
			n, err := unix.Readlinkat(srcFD, name, buf)
			if err != nil {
				return &fs.PathError{Op: "readlink", Path: path, Err: err}
			}
			if err := unix.Symlinkat(string(buf[:n]), dstFD, name); err != nil {
				return &fs.PathError{Op: "symlink", Path: path, Err: err}
			}
			if err := c.chown(dstFD, name); err != nil {
				return &fs.PathError{Op: "chown", Path: path, Err: err}
			}
		default:
			return fmt.Errorf("%s is a %s, which cannot be copied", path, modeKind(uint32(es.Mode)))
		}
	}
	return nil
}

func (c *copier) subdir(srcFD, dstFD int, rel, name string, es *unix.Stat_t) error {
	path := filepath.Join(c.root, rel, name)
	sfd, err := unix.Openat(srcFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(sfd)
	if err := unix.Mkdirat(dstFD, name, 0o700); err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	dfd, err := unix.Openat(dstFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(dfd)
	if err := c.dir(sfd, dfd, filepath.Join(rel, name), es); err != nil {
		return err
	}
	if err := c.chown(dstFD, name); err != nil {
		return &fs.PathError{Op: "chown", Path: path, Err: err}
	}
	// Permissions last, so a read-only directory can be filled.
	if err := unix.Fchmod(dfd, uint32(es.Mode&0o7777)); err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	return setTimes(dstFD, name, es)
}

func (c *copier) file(srcFD, dstFD int, path, name string, es *unix.Stat_t) (err error) {
	in, err := unix.Openat(srcFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	inF := os.NewFile(uintptr(in), path)
	defer inF.Close()
	var cur unix.Stat_t
	if err := unix.Fstat(in, &cur); err != nil || cur.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s changed while it was being copied", path)
	}
	out, err := unix.Openat(dstFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return &fs.PathError{Op: "create", Path: path, Err: err}
	}
	outF := os.NewFile(uintptr(out), path)
	defer func() {
		if cerr := outF.Close(); err == nil && cerr != nil {
			err = &fs.PathError{Op: "close", Path: path, Err: cerr}
		}
	}()
	if _, err := io.Copy(outF, &ctxReader{ctx: c.ctx, r: inF}); err != nil {
		if c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		return &fs.PathError{Op: "write", Path: path, Err: err}
	}
	if err := outF.Sync(); err != nil {
		return &fs.PathError{Op: "sync", Path: path, Err: err}
	}
	if o := c.opt.Owner; o != nil && os.Geteuid() == 0 {
		if err := unix.Fchown(out, o.UID, o.GID); err != nil {
			return &fs.PathError{Op: "chown", Path: path, Err: err}
		}
	}
	if err := unix.Fchmod(out, uint32(es.Mode&0o7777)); err != nil {
		return &fs.PathError{Op: "chmod", Path: path, Err: err}
	}
	return setTimes(dstFD, name, es)
}

// ctxReader stops a long copy of a large file when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func modeKind(m uint32) string {
	switch m & unix.S_IFMT {
	case unix.S_IFIFO:
		return "named pipe"
	case unix.S_IFSOCK:
		return "socket"
	case unix.S_IFBLK, unix.S_IFCHR:
		return "device"
	}
	return "special file"
}

// treeInfo is what a tree holds, without reading any file.
type treeInfo struct {
	// Fingerprint changes when a name, type, size, permission, modification
	// time or link target in the tree does.
	Fingerprint string
	// Coarse is the same with modification times in whole seconds. A copy
	// on a filesystem with coarser timestamps (HFS+ stores seconds) has a
	// different Fingerprint from its original, but the same Coarse one, so
	// Coarse verifies a copy while Fingerprint, compared with the same
	// filesystem's earlier value, detects change at full precision.
	Coarse string
	// Bytes is the total size of the regular files.
	Bytes int64
}

// scanTree fingerprints the tree at root. Top-level entries named in skip
// are left out. Directory modification times are not part of it: they
// change when a file is added, which the entry itself already records.
// Symbolic links are described, never followed.
func scanTree(root string, skip map[string]bool) (treeInfo, error) {
	var lines, coarse []string
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if skip[rel] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch m := info.Mode(); {
		case m.IsDir():
			line := "d\x00" + rel + "\x00" + strconv.Itoa(int(m.Perm()))
			lines, coarse = append(lines, line), append(coarse, line)
		case m.IsRegular():
			total += info.Size()
			head := "f\x00" + rel + "\x00" + strconv.Itoa(int(m.Perm())) + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00"
			lines = append(lines, head+strconv.FormatInt(info.ModTime().UnixNano(), 10))
			coarse = append(coarse, head+strconv.FormatInt(info.ModTime().Unix(), 10))
		case m&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			line := "l\x00" + rel + "\x00" + target
			lines, coarse = append(lines, line), append(coarse, line)
		default:
			return fmt.Errorf("%s is a %s, which cannot be copied", path, fileKind(m))
		}
		return nil
	})
	if err != nil {
		return treeInfo{}, err
	}
	hash := func(l []string) string {
		sort.Strings(l)
		sum := sha256.Sum256([]byte(strings.Join(l, "\n")))
		return hex.EncodeToString(sum[:])
	}
	return treeInfo{Fingerprint: hash(lines), Coarse: hash(coarse), Bytes: total}, nil
}

// isNoSpace reports whether err means a filesystem is full.
func isNoSpace(err error) bool {
	return errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT)
}
