package backup

import (
	"archive/zip"
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

const (
	// tmpPrefix names the archive being written and the staging directory.
	// Neither can be mistaken for a finished archive: an archive is only
	// ever published under its final .zip name by one atomic link.
	tmpPrefix     = ".msm-backup-"
	tmpSuffix     = ".partial"
	archiveStamp  = "2006-01-02-15-04-05"
	archiveFilePm = 0o600
	archiveDirPm  = 0o700
)

// sink is the file an archive is written to.
type sink interface {
	io.Writer
	Sync() error
	Close() error
}

// fsOps is every filesystem operation the writer makes that can fail for
// reasons outside the manager's control (a full disk, a permission). Tests
// replace it to inject those failures.
type fsOps interface {
	// CreateTemp makes a new, empty file in dir for the archive.
	CreateTemp(dir string) (sink, string, error)
	// Open opens a source file.
	Open(path string) (io.ReadCloser, error)
}

type osOps struct{}

func (osOps) CreateTemp(dir string) (sink, string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, "", err
	}
	p := filepath.Join(dir, tmpPrefix+hex.EncodeToString(b[:])+tmpSuffix)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, archiveFilePm)
	if err != nil {
		return nil, "", err
	}
	return f, p, nil
}

func (osOps) Open(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// writeArchive writes the plan to dir as a zip named for when, and
// publishes it only when it is complete: the archive is written under a
// temporary name, flushed, re-read to check it holds every planned entry,
// and linked to its final name, which never replaces an existing archive.
// On any failure, including cancellation, the temporary file is removed and
// nothing is published. It returns the final path.
func writeArchive(ctx context.Context, ops fsOps, dir string, when time.Time, entries []entry) (final string, err error) {
	f, tmp, err := ops.CreateTemp(dir)
	if err != nil {
		return "", fmt.Errorf("create archive in %s: %w", dir, err)
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
		if err != nil {
			os.Remove(tmp)
		}
	}()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return "", context.Cause(ctx)
		}
		if err := addEntry(ctx, ops, zw, e); err != nil {
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("finish archive: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("flush archive: %w", err)
	}
	closed = true
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close archive: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", context.Cause(ctx)
	}
	if err := verifyArchive(tmp, entries); err != nil {
		return "", err
	}
	final, err = publish(tmp, dir, when)
	if err != nil {
		return "", err
	}
	syncDir(dir)
	return final, nil
}

// publish links tmp to the first free name for when in dir, then removes
// tmp. A link, unlike a rename, cannot replace an archive that is already
// there.
func publish(tmp, dir string, when time.Time) (string, error) {
	base := when.Format(archiveStamp)
	for n := 0; n < 1000; n++ {
		name := base + ".zip"
		if n > 0 {
			name = fmt.Sprintf("%s-%d.zip", base, n)
		}
		final := filepath.Join(dir, name)
		err := os.Link(tmp, final)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("publish archive %s: %w", final, err)
		}
		if err := os.Remove(tmp); err != nil {
			os.Remove(final)
			return "", fmt.Errorf("publish archive %s: %w", final, err)
		}
		return final, nil
	}
	return "", fmt.Errorf("publish archive in %s: every name for %s is taken", dir, base)
}

func addEntry(ctx context.Context, ops fsOps, zw *zip.Writer, e entry) error {
	h, err := zip.FileInfoHeader(e.Info)
	if err != nil {
		return fmt.Errorf("archive %s: %w", e.Src, err)
	}
	h.Name = e.Name
	switch e.Kind {
	case kindDir:
		h.Method = zip.Store
		w, err := zw.CreateHeader(h)
		if err != nil {
			return fmt.Errorf("archive %s: %w", e.Src, err)
		}
		_ = w
		return nil
	case kindLink:
		h.Method = zip.Store
		h.SetMode(e.Info.Mode())
		w, err := zw.CreateHeader(h)
		if err != nil {
			return fmt.Errorf("archive %s: %w", e.Src, err)
		}
		if _, err := io.WriteString(w, e.Target); err != nil {
			return fmt.Errorf("archive %s: %w", e.Src, err)
		}
		return nil
	}
	h.Method = zip.Deflate
	h.SetMode(e.Info.Mode())
	in, err := ops.Open(e.Src)
	if err != nil {
		return fmt.Errorf("read %s: %w", e.Src, err)
	}
	defer in.Close()
	w, err := zw.CreateHeader(h)
	if err != nil {
		return fmt.Errorf("archive %s: %w", e.Src, err)
	}
	if _, err := io.Copy(w, &ctxReader{ctx: ctx, r: in}); err != nil {
		return fmt.Errorf("archive %s: %w", e.Src, err)
	}
	return nil
}

// ctxReader stops a long copy when the operation is canceled or times out.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, context.Cause(c.ctx)
	}
	return c.r.Read(p)
}

// verifyArchive re-reads the finished temporary archive and checks that it
// is a readable zip holding exactly the planned entries, in order.
func verifyArchive(path string, entries []entry) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("verify archive: %w", err)
	}
	defer r.Close()
	if len(r.File) != len(entries) {
		return fmt.Errorf("verify archive: it holds %d entries, expected %d", len(r.File), len(entries))
	}
	for i, f := range r.File {
		if f.Name != entries[i].Name {
			return fmt.Errorf("verify archive: entry %d is %q, expected %q", i, f.Name, entries[i].Name)
		}
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// stage copies the plan's files into a new directory so the archive can be
// compressed after saving is back on. It returns the entries to archive
// from the staged copy: the same names, modes, times and link texts, with
// each file read from its staged copy. Staged files are private to the
// manager (0600) and the archive records the original modes.
func stage(ctx context.Context, ops fsOps, stageDir string, entries []entry) ([]entry, error) {
	out := make([]entry, len(entries))
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, context.Cause(ctx)
		}
		out[i] = e
		dst := filepath.Join(stageDir, filepath.FromSlash(e.Name))
		switch e.Kind {
		case kindDir:
			if err := os.MkdirAll(dst, archiveDirPm); err != nil {
				return nil, fmt.Errorf("stage %s: %w", e.Src, err)
			}
		case kindLink:
			// Stored as link text; nothing is copied.
		default:
			if err := stageFile(ctx, ops, e, dst); err != nil {
				return nil, err
			}
			out[i].Src = dst
		}
	}
	return out, nil
}

func stageFile(ctx context.Context, ops fsOps, e entry, dst string) (err error) {
	in, err := ops.Open(e.Src)
	if err != nil {
		return fmt.Errorf("read %s: %w", e.Src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, archiveFilePm)
	if err != nil {
		return fmt.Errorf("stage %s: %w", e.Src, err)
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("stage %s: %w", e.Src, cerr)
		}
	}()
	if _, err := io.Copy(out, &ctxReader{ctx: ctx, r: in}); err != nil {
		return fmt.Errorf("stage %s: %w", e.Src, err)
	}
	return out.Sync()
}
