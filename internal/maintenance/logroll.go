// Package maintenance holds the work the legacy cron/msm file scheduled
// that is not a plain manager command: rolling a server's log into a gzip
// archive, and pruning old logs and backups. Scheduling itself stays in the
// host OS (systemd, cron, launchd); see docs/maintenance.md.
//
// # Log rolling policy
//
// A log is only rolled while its server is proven stopped, under the
// server's lock, so no process holds it open and the copy is lossless. A
// running server is skipped, never copied and truncated: the legacy
// copy-then-truncate loses every line written between the copy and the
// truncate, and a writer that does not open the log in append mode (the
// log4j appender of Minecraft 1.7 and later keeps its own file offset)
// leaves a hole of NUL bytes at its old offset. Servers from 1.7 on roll
// logs/latest.log themselves, at start and by day, into logs/*.log.gz,
// which the retention job prunes. The pre-1.7 server.log is archived the
// next time the server is stopped and rolled.
//
// The archive is written under a temporary name, flushed, re-read to prove
// it holds exactly the bytes of the log, and published with one atomic link
// that never replaces an existing archive. Only then is the log replaced,
// atomically, with the legacy note naming the archive directory. A failure
// at any step leaves the log as it was.
package maintenance

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/atomicfile"
	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/worlds"
)

var (
	ErrNotProven = errors.New("logroll: server state cannot be proven")
	ErrLogPath   = errors.New("logroll: unusable log path")
	ErrChanged   = errors.New("logroll: the log changed while it was archived")
)

const (
	// stamp is the legacy `date +%F-%H-%M-%S`.
	stamp         = "2006-01-02-15-04-05"
	rollTmpPrefix = ".msm-logroll-"
	rollTmpSuffix = ".partial"
	archiveDirPm  = 0o700
	archiveFilePm = 0o600
)

// Output is where a command writes.
type Output = worlds.Output

// Console is the part of the console manager a log roll uses.
type Console interface {
	Running(ctx context.Context, set *legacyconf.ServerSettings, out console.Output) (bool, error)
}

var _ Console = (*console.Manager)(nil)

// RollConfig connects log rolling to the host.
type RollConfig struct {
	Servers *servers.Manager
	Worlds  *worlds.Manager
	Console Console
	Clock   clock.Clock
}

// Roller rolls server logs.
type Roller struct{ cfg RollConfig }

// NewRoller returns a Roller.
func NewRoller(cfg RollConfig) (*Roller, error) {
	if cfg.Servers == nil || cfg.Worlds == nil || cfg.Console == nil || cfg.Clock == nil {
		return nil, errors.New("maintenance: servers, worlds, console and clock are required")
	}
	return &Roller{cfg: cfg}, nil
}

// ForAll rolls the log of every server in turn, as the world commands do.
func (r *Roller) ForAll(ctx context.Context, out Output, fn func(name string, out Output) error) error {
	return r.cfg.Worlds.ForAll(ctx, out, fn)
}

func printf(w io.Writer, format string, a ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", a...)
	}
}

// Roll archives a stopped server's log and empties it (msm <server> logroll).
func (r *Roller) Roll(ctx context.Context, name string, out Output) error {
	lock, set, err := r.cfg.Servers.LockServer(name)
	if err != nil {
		return err
	}
	defer lock.Release()
	owner, err := r.cfg.Servers.Owner(set)
	if err != nil {
		return err
	}
	if err := r.cfg.Servers.ActAsOwner(owner); err != nil {
		return err
	}
	running, err := r.cfg.Console.Running(ctx, set, console.Output{Out: out.Out, Err: out.Err})
	if err != nil {
		return err
	}
	if running {
		printf(out.Out, "Skipped: the server is running, and its log is only rolled while it is stopped. No change made.")
		return nil
	}
	if err := servers.RequireStopped(name, r.cfg.Servers.Probe(ctx, set)); err != nil {
		return fmt.Errorf("%w: %v", ErrNotProven, err)
	}
	return r.roll(ctx, set, out)
}

func (r *Roller) roll(ctx context.Context, set *legacyconf.ServerSettings, out Output) error {
	rel, err := filepath.Rel(set.Dir, set.Get("LOG_PATH"))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: LOG_PATH %s is not inside the server directory %s", ErrLogPath, set.Get("LOG_PATH"), set.Dir)
	}
	logPath, err := safepath.Contain(set.Dir, rel)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLogPath, err)
	}
	info, err := os.Lstat(logPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		printf(out.Out, "No new log entries to roll. No change made.")
		return nil
	case err != nil:
		return fmt.Errorf("inspect log %s: %w", logPath, err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrLogPath, logPath)
	}
	archives, err := backup.ArchiveRoot(r.cfg.Servers, "LOG_ARCHIVE_PATH")
	if err != nil {
		return err
	}
	dir := filepath.Join(archives, set.Name)
	if err := os.MkdirAll(dir, archiveDirPm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	when := r.cfg.Clock.Now()
	final := filepath.Join(dir, set.Name+"-"+when.Format(stamp)+".log.gz")
	printf(out.Out, "Rolling server logs...")
	rolled, err := archiveLog(ctx, logPath, info, dir, final)
	if err != nil {
		return err
	}
	if !rolled {
		printf(out.Out, "No new log entries to roll. No change made.")
		return nil
	}
	note := fmt.Sprintf("Previous logs can be found at \"%s\"\n", dir)
	if err := atomicfile.Write(logPath, []byte(note), archiveFilePm); err != nil {
		return fmt.Errorf("the log is archived at %s but could not be emptied, so it is unchanged: %w", final, err)
	}
	printf(out.Out, "Done. The log is archived at %s", final)
	return nil
}

// archiveLog gzips the log into final and reports whether it did. A log of
// one line or none is not rolled, as in the legacy command. The log is
// never changed here.
func archiveLog(ctx context.Context, logPath string, before os.FileInfo, dir, final string) (rolled bool, err error) {
	src, err := os.OpenFile(logPath, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer src.Close()
	if opened, err := src.Stat(); err != nil || !os.SameFile(before, opened) {
		return false, fmt.Errorf("%w: %s was replaced while it was opened", ErrChanged, logPath)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return false, err
	}
	tmp := filepath.Join(dir, rollTmpPrefix+hex.EncodeToString(rnd[:])+rollTmpSuffix)
	dst, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, archiveFilePm)
	if err != nil {
		return false, fmt.Errorf("create archive in %s: %w", dir, err)
	}
	closed := false
	defer func() {
		if !closed {
			dst.Close()
		}
		os.Remove(tmp) // gone after a publish, because the link keeps the data
	}()
	zw := gzip.NewWriter(dst)
	sum := sha256.New()
	lines, size, err := copyCounting(ctx, io.MultiWriter(zw, sum), src)
	if err != nil {
		return false, fmt.Errorf("archive %s: %w", logPath, err)
	}
	if lines <= 1 {
		return false, nil
	}
	if err := zw.Close(); err != nil {
		return false, fmt.Errorf("finish archive: %w", err)
	}
	if err := dst.Sync(); err != nil {
		return false, fmt.Errorf("flush archive: %w", err)
	}
	closed = true
	if err := dst.Close(); err != nil {
		return false, fmt.Errorf("close archive: %w", err)
	}
	if err := verifyGzip(tmp, size, sum.Sum(nil)); err != nil {
		return false, err
	}
	after, err := os.Lstat(logPath)
	if err != nil || !os.SameFile(before, after) || after.Size() != size || !after.ModTime().Equal(before.ModTime()) {
		return false, fmt.Errorf("%w: %s", ErrChanged, logPath)
	}
	if err := ctx.Err(); err != nil {
		return false, context.Cause(ctx)
	}
	if err := os.Link(tmp, final); err != nil {
		return false, fmt.Errorf("publish %s: %w (an archive with that name exists, or the directory is not writable)", final, err)
	}
	syncDir(dir)
	return true, nil
}

// copyCounting copies src to dst and counts its lines, as wc -l does.
func copyCounting(ctx context.Context, dst io.Writer, src io.Reader) (lines, size int64, err error) {
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, context.Cause(ctx)
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			lines += int64(bytes.Count(buf[:n], []byte{'\n'}))
			size += int64(n)
			if _, err := dst.Write(buf[:n]); err != nil {
				return 0, 0, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return lines, size, nil
		}
		if rerr != nil {
			return 0, 0, rerr
		}
	}
}

// verifyGzip re-reads the archive and proves it decompresses to exactly
// size bytes with the given SHA-256.
func verifyGzip(path string, size int64, want []byte) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("verify archive: %w", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return fmt.Errorf("verify archive: %w", err)
	}
	sum := sha256.New()
	n, err := io.Copy(sum, zr)
	if err != nil {
		return fmt.Errorf("verify archive: %w", err)
	}
	if n != size || !bytes.Equal(sum.Sum(nil), want) {
		return fmt.Errorf("verify archive: it does not hold the log's %d bytes", size)
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
