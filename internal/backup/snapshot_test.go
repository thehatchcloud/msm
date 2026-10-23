package backup

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// snapWorld makes a world directory under root with the files a snapshot
// has to carry: nested directories, an empty directory, a link, odd modes
// and a read-only directory. It returns the world's path.
func snapWorld(t *testing.T, root string) string {
	t.Helper()
	w := filepath.Join(root, "alpha")
	at := time.Date(2025, 3, 4, 5, 6, 7, 123456789, time.UTC)
	write(t, filepath.Join(w, "level.dat"), "level one", 0o644)
	write(t, filepath.Join(w, "region", "r.0.0.mca"), strings.Repeat("region ", 100), 0o640)
	write(t, filepath.Join(w, "playerdata", "p.dat"), "player", 0o600)
	write(t, filepath.Join(w, "ro", "frozen.txt"), "frozen", 0o444)
	if err := os.MkdirAll(filepath.Join(w, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("level.dat", filepath.Join(w, "latest-level")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"level.dat", "region/r.0.0.mca", "playerdata/p.dat", "ro/frozen.txt"} {
		if err := os.Chtimes(filepath.Join(w, p), at, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(w, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeRemovable(root) })
	return w
}

// snapDir makes the directory snapshots are published in. A snapshot keeps
// the world's read-only directories, which an unprivileged user cannot
// remove until they are made writable, so the test's cleanup does that first.
func snapDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeRemovable(dir) })
}

func planOf(t *testing.T, w string) []entry {
	t.Helper()
	p, err := buildPlan(w, filepath.Base(w), policy{})
	if err != nil {
		t.Fatal(err)
	}
	return p.Entries
}

func snap(t *testing.T, ops fsOps, dir, w string, at time.Time) snapshotStats {
	t.Helper()
	st, err := writeSnapshot(context.Background(), ops, dir, "alpha", at, planOf(t, w))
	if err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	return st
}

func ino(t *testing.T, p string) (fs.FileInfo, uint64) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info, info.Sys().(*syscall.Stat_t).Ino
}

func nlink(t *testing.T, p string) uint64 {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(info.Sys().(*syscall.Stat_t).Nlink)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

var t1 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// The first snapshot is a complete copy with the world's names, modes, times
// and links, laid out as <timestamp>/<world>/..., and latest names it.
func TestSnapshotFirst(t *testing.T) {
	root := t.TempDir()
	w := snapWorld(t, filepath.Join(root, "src"))
	dir := filepath.Join(root, "snapshots")
	snapDir(t, dir)
	st := snap(t, osOps{}, dir, w, t1)

	want := filepath.Join(dir, "2026-01-02-03-04-05")
	if st.Path != want {
		t.Fatalf("path = %s, want %s", st.Path, want)
	}
	if st.Copied != 4 || st.Linked != 0 {
		t.Errorf("copied %d linked %d, want 4 and 0", st.Copied, st.Linked)
	}
	if target, err := os.Readlink(filepath.Join(dir, "latest")); err != nil || target != "2026-01-02-03-04-05" {
		t.Errorf("latest -> %q, %v", target, err)
	}
	copy := filepath.Join(want, "alpha")
	if got := readFile(t, filepath.Join(copy, "region", "r.0.0.mca")); got != strings.Repeat("region ", 100) {
		t.Errorf("region content %q", got)
	}
	for rel, mode := range map[string]fs.FileMode{"level.dat": 0o644, "region/r.0.0.mca": 0o640, "playerdata/p.dat": 0o600,
		"ro/frozen.txt": 0o444, "ro": fs.ModeDir | 0o555, "empty": fs.ModeDir | 0o755, "region": fs.ModeDir | 0o755} {
		info, err := os.Lstat(filepath.Join(copy, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if info.Mode() != mode {
			t.Errorf("%s mode = %v, want %v", rel, info.Mode(), mode)
		}
	}
	srcInfo, _ := os.Lstat(filepath.Join(w, "level.dat"))
	gotInfo, _ := os.Lstat(filepath.Join(copy, "level.dat"))
	if !gotInfo.ModTime().Equal(srcInfo.ModTime()) {
		t.Errorf("mtime = %v, want %v", gotInfo.ModTime(), srcInfo.ModTime())
	}
	srcDir, _ := os.Lstat(filepath.Join(w, "region"))
	gotDir, _ := os.Lstat(filepath.Join(copy, "region"))
	if !gotDir.ModTime().Equal(srcDir.ModTime()) {
		t.Errorf("directory mtime = %v, want %v", gotDir.ModTime(), srcDir.ModTime())
	}
	if target, err := os.Readlink(filepath.Join(copy, "latest-level")); err != nil || target != "level.dat" {
		t.Errorf("link -> %q, %v (links are stored, not followed)", target, err)
	}
}

// A second snapshot hard-links what did not change, copies what did, and
// never alters the first: not its content, its modes, its times or its names.
func TestSnapshotSharesUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	w := snapWorld(t, filepath.Join(root, "src"))
	dir := filepath.Join(root, "snapshots")
	snapDir(t, dir)
	first := snap(t, osOps{}, dir, w, t1)

	type before struct {
		mode  fs.FileMode
		mtime time.Time
		data  string
	}
	record := func(snapRoot string) map[string]before {
		out := map[string]before{}
		filepath.WalkDir(snapRoot, func(p string, d fs.DirEntry, err error) error {
			info, _ := d.Info()
			b := before{mode: info.Mode(), mtime: info.ModTime()}
			if info.Mode().IsRegular() {
				b.data = readFile(t, p)
			}
			rel, _ := filepath.Rel(snapRoot, p)
			out[rel] = b
			return nil
		})
		return out
	}
	old := record(first.Path)

	// Change one file's content (same size, new time), add one, remove one,
	// and change another's mode only.
	write(t, filepath.Join(w, "level.dat"), "level TWO", 0o644)
	later := time.Date(2025, 3, 4, 5, 6, 9, 0, time.UTC)
	os.Chtimes(filepath.Join(w, "level.dat"), later, later)
	write(t, filepath.Join(w, "new.txt"), "new", 0o644)
	os.Remove(filepath.Join(w, "playerdata", "p.dat"))
	os.Chmod(filepath.Join(w, "region", "r.0.0.mca"), 0o600)

	second := snap(t, osOps{}, dir, w, t1.Add(time.Hour))
	a, b := filepath.Join(first.Path, "alpha"), filepath.Join(second.Path, "alpha")

	// Unchanged: same inode, shared.
	_, ia := ino(t, filepath.Join(a, "ro", "frozen.txt"))
	_, ib := ino(t, filepath.Join(b, "ro", "frozen.txt"))
	if ia != ib || nlink(t, filepath.Join(b, "ro", "frozen.txt")) != 2 {
		t.Errorf("an unchanged file is not hard-linked (inodes %d, %d)", ia, ib)
	}
	// Changed content, and changed mode: new files.
	for _, rel := range []string{"level.dat", "region/r.0.0.mca"} {
		_, ia := ino(t, filepath.Join(a, rel))
		_, ib := ino(t, filepath.Join(b, rel))
		if ia == ib {
			t.Errorf("%s changed but shares its inode with the older snapshot", rel)
		}
	}
	if readFile(t, filepath.Join(b, "level.dat")) != "level TWO" || readFile(t, filepath.Join(a, "level.dat")) != "level one" {
		t.Errorf("contents: new %q, old %q", readFile(t, filepath.Join(b, "level.dat")), readFile(t, filepath.Join(a, "level.dat")))
	}
	if info, _ := os.Lstat(filepath.Join(b, "region", "r.0.0.mca")); info.Mode().Perm() != 0o600 {
		t.Errorf("new snapshot mode = %v, want 0600", info.Mode().Perm())
	}
	if !exists(filepath.Join(b, "new.txt")) || exists(filepath.Join(b, "playerdata", "p.dat")) || !exists(filepath.Join(a, "playerdata", "p.dat")) {
		t.Errorf("added/removed files are wrong")
	}
	if second.Linked != 1 || second.Copied != 3 {
		t.Errorf("linked %d copied %d, want 1 and 3 (frozen shared; level, region, new copied)", second.Linked, second.Copied)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "latest")); target != "2026-01-02-04-04-05" {
		t.Errorf("latest -> %q", target)
	}
	// The first snapshot is exactly as it was.
	now := record(first.Path)
	for rel, o := range old {
		n, ok := now[rel]
		if !ok || n != o {
			t.Errorf("older snapshot changed at %s: %+v -> %+v", rel, o, n)
		}
	}
	if len(now) != len(old) {
		t.Errorf("older snapshot has %d entries, had %d", len(now), len(old))
	}
}

// A hard link makes no copy, so a link that cannot be made falls back to
// copying: always when the filesystem refuses links across the two
// snapshots, and for one file when it has too many links.
func TestSnapshotLinkFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		wantCopied int
		wantNote   bool
	}{
		"different filesystem":  {syscall.EXDEV, 4, true},
		"no hard links":         {syscall.EPERM, 4, true},
		"too many links (file)": {syscall.EMLINK, 4, false},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			w := snapWorld(t, filepath.Join(root, "src"))
			dir := filepath.Join(root, "snapshots")
			snapDir(t, dir)
			snap(t, osOps{}, dir, w, t1)
			attempts := 0
			st := snap(t, faultOps{link: func(o, n string) error { attempts++; return tc.err }}, dir, w, t1.Add(time.Hour))
			if st.Copied != tc.wantCopied || st.Linked != 0 {
				t.Errorf("copied %d linked %d", st.Copied, st.Linked)
			}
			if (st.NoLinks != "") != tc.wantNote {
				t.Errorf("note = %q", st.NoLinks)
			}
			if tc.wantNote && attempts != 1 {
				t.Errorf("tried %d links after the filesystem refused them; want 1", attempts)
			}
			if got := readFile(t, filepath.Join(st.Path, "alpha", "level.dat")); got != "level one" {
				t.Errorf("content %q", got)
			}
		})
	}
}

// The world may be on another filesystem than the snapshots; the snapshots
// share files with each other all the same.
func TestSnapshotSourceOnAnotherFilesystem(t *testing.T) {
	shm := "/dev/shm"
	var a, b syscall.Stat_t
	root := t.TempDir()
	if syscall.Stat(shm, &a) != nil || syscall.Stat(root, &b) != nil || a.Dev == b.Dev {
		t.Skip("no second filesystem to read the world from")
	}
	src, err := os.MkdirTemp(shm, "msm-snap-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { makeRemovable(src); os.RemoveAll(src) })
	w := snapWorld(t, src)
	dir := filepath.Join(root, "snapshots")
	snapDir(t, dir)
	snap(t, osOps{}, dir, w, t1)
	st := snap(t, osOps{}, dir, w, t1.Add(time.Hour))
	if st.Linked != 4 || st.Copied != 0 {
		t.Errorf("linked %d copied %d, want 4 and 0", st.Linked, st.Copied)
	}
}

// A "latest" that is not a link to a snapshot of this world is not trusted:
// nothing is linked through it and the snapshot copies everything.
func TestSnapshotIgnoresBadLatest(t *testing.T) {
	root := t.TempDir()
	w := snapWorld(t, filepath.Join(root, "src"))
	outside := filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "alpha", "level.dat"), "level one", 0o644)
	for name, setup := range map[string]func(dir string){
		"dangling":     func(dir string) { os.Symlink("2000-01-01-00-00-00", filepath.Join(dir, "latest")) },
		"absolute":     func(dir string) { os.Symlink(outside, filepath.Join(dir, "latest")) },
		"parent":       func(dir string) { os.Symlink("../outside", filepath.Join(dir, "latest")) },
		"regular file": func(dir string) { os.WriteFile(filepath.Join(dir, "latest"), []byte("x"), 0o600) },
		"other world": func(dir string) {
			os.MkdirAll(filepath.Join(dir, "snap", "beta"), 0o700)
			os.Symlink("snap", filepath.Join(dir, "latest"))
		},
		"hidden target": func(dir string) {
			os.MkdirAll(filepath.Join(dir, ".snap", "alpha"), 0o700)
			os.Symlink(".snap", filepath.Join(dir, "latest"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "snapshots")
			snapDir(t, dir)
			setup(dir)
			st, err := writeSnapshot(context.Background(), osOps{}, dir, "alpha", t1, planOf(t, w))
			if err != nil {
				t.Fatal(err)
			}
			if st.Linked != 0 || st.Copied != 4 {
				t.Errorf("linked %d copied %d", st.Linked, st.Copied)
			}
			if st.NoLinks == "" {
				t.Errorf("no explanation for copying everything")
			}
			if target, _ := os.Readlink(filepath.Join(dir, "latest")); target != "2026-01-02-03-04-05" {
				t.Errorf("latest -> %q afterwards", target)
			}
		})
	}
}

// A snapshot that fails is not published and does not move latest.
func TestSnapshotFailureLeavesNothing(t *testing.T) {
	root := t.TempDir()
	w := snapWorld(t, filepath.Join(root, "src"))
	dir := filepath.Join(root, "snapshots")
	snapDir(t, dir)
	first := snap(t, osOps{}, dir, w, t1)
	write(t, filepath.Join(w, "region", "r.0.0.mca"), "changed so it is copied", 0o640)

	for name, ops := range map[string]fsOps{
		"unreadable": faultOps{open: func(p string) (io.ReadCloser, error) {
			if strings.HasSuffix(p, "r.0.0.mca") {
				return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
			}
			return os.Open(p)
		}},
		"changed while copied": faultOps{open: func(p string) (io.ReadCloser, error) {
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(p, "r.0.0.mca") {
				// Hide Stat so the size comes from the plan, then grow the file.
				os.WriteFile(p, []byte("changed so it is copied and then some"), 0o640)
				return struct{ io.ReadCloser }{f}, nil
			}
			return f, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := writeSnapshot(context.Background(), ops, dir, "alpha", t1.Add(2*time.Hour), planOf(t, w))
			if err == nil {
				t.Fatal("no error")
			}
			if target, _ := os.Readlink(filepath.Join(dir, "latest")); target != filepath.Base(first.Path) {
				t.Errorf("latest moved to %q", target)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Errorf("directory holds %v", names)
			}
		})
	}

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ops := faultOps{open: func(p string) (io.ReadCloser, error) {
			if strings.HasSuffix(p, "r.0.0.mca") {
				cancel()
			}
			return os.Open(p)
		}}
		_, err := writeSnapshot(ctx, ops, dir, "alpha", t1.Add(3*time.Hour), planOf(t, w))
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 2 {
			t.Errorf("%d entries in the snapshot directory", len(entries))
		}
	})
}

// Two snapshots in one second keep both; the second never replaces the first.
func TestSnapshotSameSecond(t *testing.T) {
	root := t.TempDir()
	w := snapWorld(t, filepath.Join(root, "src"))
	dir := filepath.Join(root, "snapshots")
	snapDir(t, dir)
	a := snap(t, osOps{}, dir, w, t1)
	write(t, filepath.Join(a.Path, "alpha", "marker"), "first", 0o644)
	b := snap(t, osOps{}, dir, w, t1)
	if filepath.Base(b.Path) != "2026-01-02-03-04-05-1" {
		t.Errorf("second snapshot = %s", b.Path)
	}
	if readFile(t, filepath.Join(a.Path, "alpha", "marker")) != "first" {
		t.Errorf("the first snapshot was replaced")
	}
	if target, _ := os.Readlink(filepath.Join(dir, "latest")); target != "2026-01-02-03-04-05-1" {
		t.Errorf("latest -> %q", target)
	}
}

// A real directory named latest is not replaced; the snapshot is kept and
// the error says so.
func TestSnapshotLatestIsDirectory(t *testing.T) {
	root := t.TempDir()
	w := snapWorld(t, filepath.Join(root, "src"))
	dir := filepath.Join(root, "snapshots")
	snapDir(t, dir)
	os.MkdirAll(filepath.Join(dir, "latest", "keep"), 0o700)
	st, err := writeSnapshot(context.Background(), osOps{}, dir, "alpha", t1, planOf(t, w))
	if err == nil || !strings.Contains(err.Error(), "was not updated") {
		t.Fatalf("err = %v", err)
	}
	if !exists(filepath.Join(st.Path, "alpha", "level.dat")) || !exists(filepath.Join(dir, "latest", "keep")) {
		t.Errorf("snapshot or the directory is gone")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("leftovers: %v", entries)
	}
}

// sweep removes an interrupted snapshot, read-only directories and all, and
// only that: not a world whose name looks similar.
func TestSweepRemovesPartialSnapshots(t *testing.T) {
	base := t.TempDir()
	partial := filepath.Join(base, "alpha", tmpPrefix+snapshotTmpKind+"0123456789abcdef"+tmpSuffix)
	write(t, filepath.Join(partial, "alpha", "ro", "f"), "x", 0o444)
	os.Chmod(filepath.Join(partial, "alpha", "ro"), 0o555)
	lookalike := filepath.Join(base, "alpha", tmpPrefix+"mine"+tmpSuffix)
	write(t, filepath.Join(lookalike, "keep"), "keep", 0o644)
	atBase := filepath.Join(base, tmpPrefix+snapshotTmpKind+"fedcba9876543210"+tmpSuffix)
	write(t, filepath.Join(atBase, "f"), "x", 0o644)
	good := filepath.Join(base, "alpha", "2026-01-02-03-04-05", "alpha", "level.dat")
	write(t, good, "data", 0o644)

	removed, err := sweep(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || exists(partial) || exists(atBase) {
		t.Errorf("removed %v", removed)
	}
	if !exists(lookalike) || !exists(good) {
		t.Errorf("sweep removed something it does not own")
	}
}
