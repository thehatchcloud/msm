package maintenance

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/servers"
)

// Category is one kind of file retention removes. Each replaces one
// `find ... | xargs rm` line of the legacy cron/msm.
type Category string

const (
	// ServerLogs is <server>/logs/*.log.gz, the logs Minecraft 1.7 and
	// later roll themselves.
	ServerLogs Category = "server-logs"
	// Backups is <BACKUP_ARCHIVE_PATH>/<server>/*.zip, complete backups.
	Backups Category = "backups"
	// LogArchives is <LOG_ARCHIVE_PATH>/<server>/*.log.gz, rolled logs.
	LogArchives Category = "log-archives"
	// WorldBackups is <WORLD_ARCHIVE_PATH>/<server>/<world>/*.zip.
	WorldBackups Category = "world-backups"
)

// Categories lists every category, in the order the legacy cron ran them.
var Categories = []Category{ServerLogs, Backups, LogArchives, WorldBackups}

// DefaultDays is the legacy cron's `-mtime +30`.
const DefaultDays = 30

var (
	ErrUnsafeRoot = errors.New("prune: unsafe root")
	ErrOptions    = errors.New("prune: invalid option")
	ErrPrune      = errors.New("prune: some files could not be removed")
)

// archiveName is the name backups publish: a timestamped ZIP. Only these
// are candidates in a backup directory, so a ZIP an administrator put there
// by hand is never removed.
var archiveName = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{2}\.zip$`)

// PruneOptions select what Prune does.
type PruneOptions struct {
	// Days is how old, in whole days by modification time, a file must be
	// to be removed: the legacy `find -mtime +<Days>`.
	Days int
	// Keep is how many of the newest recoverable archives a backup
	// directory always keeps, however old; at least 1.
	Keep int
	// DryRun reports what would be removed and removes nothing.
	DryRun bool
	// Only limits pruning to these categories; none means all.
	Only []Category
}

// PruneConfig connects pruning to the host.
type PruneConfig struct {
	Servers *servers.Manager
	Clock   clock.Clock
}

// Pruner removes old logs and backups.
type Pruner struct{ cfg PruneConfig }

// NewPruner returns a Pruner.
func NewPruner(cfg PruneConfig) (*Pruner, error) {
	if cfg.Servers == nil || cfg.Clock == nil {
		return nil, errors.New("maintenance: servers and clock are required")
	}
	return &Pruner{cfg: cfg}, nil
}

// ParseCategories parses a comma-separated category list.
func ParseCategories(list string) ([]Category, error) {
	var out []Category
	for _, f := range strings.Split(list, ",") {
		c := Category(strings.TrimSpace(f))
		if !validCategory(c) {
			return nil, fmt.Errorf("%w: unknown category %q (use %s)", ErrOptions, f, joinCategories(Categories))
		}
		out = append(out, c)
	}
	return out, nil
}

func validCategory(c Category) bool {
	for _, k := range Categories {
		if c == k {
			return true
		}
	}
	return false
}

func joinCategories(cs []Category) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, ", ")
}

func (o PruneOptions) selected(c Category) bool {
	if len(o.Only) == 0 {
		return true
	}
	for _, x := range o.Only {
		if x == c {
			return true
		}
	}
	return false
}

func (o PruneOptions) validate() error {
	switch {
	case o.Days < 1 || o.Days > 36500:
		return fmt.Errorf("%w: --days must be between 1 and 36500", ErrOptions)
	case o.Keep < 1:
		return fmt.Errorf("%w: --keep must be at least 1; the newest recoverable backup is never removed", ErrOptions)
	}
	for _, c := range o.Only {
		if !validCategory(c) {
			return fmt.Errorf("%w: unknown category %q", ErrOptions, c)
		}
	}
	return nil
}

// root is one directory retention works below. Every operation goes
// through an os.Root, which cannot be led outside the directory by a
// symbolic link or ".." even if one is swapped in while pruning runs.
type root struct {
	cat  Category
	path string
	fs   *os.Root
}

// minRootDepth rejects /, /opt and the like: a retention root names a
// directory made for it, at least two components deep.
const minRootDepth = 2

// checkRoots refuses roots that are not a real directory made for the job:
// not absolute and clean, a symbolic link, too shallow, or overlapping
// another root. A root that does not exist yet has nothing to prune.
func checkRoots(named map[string]string) error {
	names := make([]string, 0, len(named))
	for n := range named {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := named[n]
		if err := safepath.ValidateConfiguredRoot(p); err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnsafeRoot, n, err)
		}
		if depth := len(strings.Split(strings.Trim(p, string(filepath.Separator)), string(filepath.Separator))); depth < minRootDepth {
			return fmt.Errorf("%w: %s %s is too close to the filesystem root; give it a directory of its own", ErrUnsafeRoot, n, p)
		}
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return fmt.Errorf("%w: %s %s: %v", ErrUnsafeRoot, n, p, err)
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s %s is a symbolic link; name the directory itself", ErrUnsafeRoot, n, p)
		case !info.IsDir():
			return fmt.Errorf("%w: %s %s is not a directory", ErrUnsafeRoot, n, p)
		}
		// Compare real locations: a parent that is a link cannot hide an
		// overlap.
		if real, err := filepath.EvalSymlinks(p); err == nil {
			named[n] = real
		}
	}
	for i, a := range names {
		for _, b := range names[i+1:] {
			pa, pb := named[a], named[b]
			if overlaps(pa, pb) {
				return fmt.Errorf("%w: %s %s and %s %s overlap; each must be its own directory", ErrUnsafeRoot, a, pa, b, pb)
			}
		}
	}
	return nil
}

func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
}

// Prune removes files older than o.Days that the selected categories
// cover, and reports each. A backup directory always keeps its o.Keep
// newest recoverable archives: if none of its archives verifies, nothing
// in it is removed. Each server is pruned under its lock, so a backup
// cannot publish an archive while its directory is read; the archives of a
// server that no longer exists are pruned without one.
//
// A failure on one file or directory is reported and does not stop the
// others; the result then wraps ErrPrune.
func (p *Pruner) Prune(ctx context.Context, o PruneOptions, out Output) error {
	if err := o.validate(); err != nil {
		return err
	}
	sm := p.cfg.Servers
	paths := map[string]string{"SERVER_STORAGE_PATH": sm.Root()}
	settings := map[Category]string{Backups: "BACKUP_ARCHIVE_PATH", LogArchives: "LOG_ARCHIVE_PATH", WorldBackups: "WORLD_ARCHIVE_PATH"}
	for _, c := range Categories {
		name, ok := settings[c]
		if !ok || !o.selected(c) {
			continue
		}
		dir, err := backup.ArchiveRoot(sm, name)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsafeRoot, err)
		}
		paths[name] = dir
	}
	if err := checkRoots(paths); err != nil {
		return err
	}
	r := &run{p: p, o: o, out: out, now: p.cfg.Clock.Now()}
	defer r.close()
	if o.selected(ServerLogs) {
		if err := r.open(ServerLogs, paths["SERVER_STORAGE_PATH"]); err != nil {
			return err
		}
	}
	for c, name := range settings {
		if o.selected(c) {
			if err := r.open(c, paths[name]); err != nil {
				return err
			}
		}
	}
	names, err := sm.Names()
	if err != nil {
		return err
	}
	if r.dry() {
		printf(out.Out, "Dry run: nothing will be removed.")
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		r.server(ctx, name)
	}
	r.orphans(ctx, names)
	return r.finish()
}

// run is one Prune call.
type run struct {
	p     *Pruner
	o     PruneOptions
	out   Output
	now   time.Time
	roots map[Category]*root

	removed, kept int
	bytes         int64
	errs          []string
}

func (r *run) dry() bool { return r.o.DryRun }

func (r *run) open(c Category, path string) error {
	fsRoot, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: open %s: %v", ErrUnsafeRoot, path, err)
	}
	if r.roots == nil {
		r.roots = map[Category]*root{}
	}
	r.roots[c] = &root{cat: c, path: path, fs: fsRoot}
	return nil
}

func (r *run) close() {
	for _, rt := range r.roots {
		rt.fs.Close()
	}
}

func (r *run) fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.errs = append(r.errs, msg)
	printf(r.out.Err, "msm: warning: %s", msg)
}

func (r *run) finish() error {
	verb := "Removed"
	if r.dry() {
		verb = "Would remove"
	}
	printf(r.out.Out, "%s %d files (%s); kept %d recent backups.", verb, r.removed, humanSize(r.bytes), r.kept)
	if len(r.errs) > 0 {
		return fmt.Errorf("%w: %d problems, the first: %s", ErrPrune, len(r.errs), r.errs[0])
	}
	return nil
}

// server prunes one server's files under its lock.
func (r *run) server(ctx context.Context, name string) {
	if !r.dry() {
		lock, set, err := r.p.cfg.Servers.LockServer(name)
		if err != nil {
			r.fail("server %s: %v", name, err)
			return
		}
		defer lock.Release()
		owner, err := r.p.cfg.Servers.Owner(set)
		if err == nil {
			err = r.p.cfg.Servers.ActAsOwner(owner)
		}
		if err != nil {
			r.fail("server %s: %v", name, err)
			return
		}
	}
	r.tree(ctx, name)
}

// tree prunes the files of one server name in every selected category.
func (r *run) tree(ctx context.Context, name string) {
	if rt := r.roots[ServerLogs]; rt != nil {
		r.logs(rt, filepath.Join(name, "logs"))
	}
	if rt := r.roots[LogArchives]; rt != nil {
		r.logs(rt, name)
	}
	if rt := r.roots[Backups]; rt != nil {
		r.archives(rt, name)
	}
	if rt := r.roots[WorldBackups]; rt != nil {
		worlds, err := r.subdirs(rt, name)
		if err != nil {
			r.fail("%v", err)
			return
		}
		for _, w := range worlds {
			if ctx.Err() != nil {
				return
			}
			r.archives(rt, filepath.Join(name, w))
		}
	}
}

// orphans prunes archive directories whose server no longer exists. Their
// newest recoverable backups are kept too: they may be all that is left of
// the server.
func (r *run) orphans(ctx context.Context, live []string) {
	known := map[string]bool{}
	for _, n := range live {
		known[n] = true
	}
	for _, c := range []Category{LogArchives, Backups, WorldBackups} {
		rt := r.roots[c]
		if rt == nil {
			continue
		}
		dirs, err := r.subdirs(rt, ".")
		if err != nil {
			r.fail("%v", err)
			continue
		}
		for _, d := range dirs {
			if known[d] || ctx.Err() != nil {
				continue
			}
			switch c {
			case LogArchives:
				r.logs(rt, d)
			case Backups:
				r.archives(rt, d)
			default:
				worlds, err := r.subdirs(rt, d)
				if err != nil {
					r.fail("%v", err)
					continue
				}
				for _, w := range worlds {
					r.archives(rt, filepath.Join(d, w))
				}
			}
		}
	}
}

// subdirs lists the real directories directly inside rel. A missing
// directory has none; a symbolic link is not a directory here.
func (r *run) subdirs(rt *root, rel string) ([]string, error) {
	entries, err := readDir(rt, rel)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsDir() && safepath.ValidateName(e.Name()) == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// readDir lists rel below the root, which must be a real directory.
func readDir(rt *root, rel string) ([]fs.DirEntry, error) {
	info, err := rt.fs.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", filepath.Join(rt.path, rel), err)
	}
	if !info.IsDir() {
		return nil, nil
	}
	d, err := rt.fs.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Join(rt.path, rel), err)
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", filepath.Join(rt.path, rel), err)
	}
	return entries, nil
}

// old reports whether a file's age is more than Days whole days, as
// find -mtime +Days does. A modification time in the future is never old.
func (r *run) old(info fs.FileInfo) bool {
	return r.now.Sub(info.ModTime()) >= time.Duration(r.o.Days+1)*24*time.Hour
}

// logs prunes *.log.gz regular files directly in rel.
func (r *run) logs(rt *root, rel string) {
	entries, err := readDir(rt, rel)
	if err != nil {
		r.fail("%v", err)
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".log.gz") {
			continue
		}
		r.consider(rt, filepath.Join(rel, e.Name()))
	}
}

// consider removes one file if it is old enough.
func (r *run) consider(rt *root, rel string) {
	info, err := rt.fs.Lstat(rel)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			r.fail("inspect %s: %v", filepath.Join(rt.path, rel), err)
		}
		return
	}
	if !info.Mode().IsRegular() || !r.old(info) {
		return
	}
	r.remove(rt, rel, info)
}

func (r *run) remove(rt *root, rel string, info fs.FileInfo) {
	full := filepath.Join(rt.path, rel)
	age := int(r.now.Sub(info.ModTime()) / (24 * time.Hour))
	if r.dry() {
		printf(r.out.Out, "would remove %s (%d days old, %s)", full, age, humanSize(info.Size()))
	} else {
		if err := rt.fs.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.fail("remove %s: %v", full, err)
			return
		}
		printf(r.out.Out, "removed %s (%d days old, %s)", full, age, humanSize(info.Size()))
	}
	r.removed++
	r.bytes += info.Size()
}

// archives prunes the timestamped ZIPs directly in rel, keeping the
// newest recoverable ones.
func (r *run) archives(rt *root, rel string) {
	entries, err := readDir(rt, rel)
	if err != nil {
		r.fail("%v", err)
		return
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && archiveName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return
	}
	// The name is a timestamp, so name order is age order.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	keep := map[string]bool{}
	for _, n := range names {
		if len(keep) == r.o.Keep {
			break
		}
		if err := verifyZip(rt.fs, filepath.Join(rel, n)); err != nil {
			printf(r.out.Err, "msm: warning: %s is not recoverable: %v", filepath.Join(rt.path, rel, n), err)
			continue
		}
		keep[n] = true
	}
	if len(keep) == 0 {
		printf(r.out.Err, "msm: warning: no archive in %s can be recovered, so none of them is removed",
			filepath.Join(rt.path, rel))
		return
	}
	r.kept += len(keep)
	for _, n := range names {
		if !keep[n] {
			r.consider(rt, filepath.Join(rel, n))
		}
	}
}

// verifyZip proves an archive is a finished ZIP: its central directory,
// written last, is present and lists at least one entry. Backups publish an
// archive only after it was complete and verified, so one that fails this
// was damaged afterwards.
func verifyZip(r *os.Root, rel string) error {
	f, err := r.Open(rel)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return err
	}
	if len(zr.File) == 0 {
		return errors.New("it holds no entries")
	}
	return nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
