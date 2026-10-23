package backup

import (
	"archive/zip"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// extract unpacks an archive into the new directory dest. It exists only to
// test recovery: the manager has no restore command, and this helper is not
// part of it. It refuses entries that would leave dest.
func extract(t *testing.T, archive, dest string) {
	t.Helper()
	r, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range r.File {
		path := filepath.Join(dest, filepath.FromSlash(f.Name))
		if rel, err := filepath.Rel(dest, path); err != nil || strings.HasPrefix(rel, "..") {
			t.Fatalf("entry %q leaves the restore directory", f.Name)
		}
		mode := f.Mode()
		switch {
		case f.FileInfo().IsDir():
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(path, mode.Perm())
		case mode&fs.ModeSymlink != 0:
			rc, _ := f.Open()
			target, _ := io.ReadAll(rc)
			rc.Close()
			if err := os.Symlink(string(target), path); err != nil {
				t.Fatal(err)
			}
		default:
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, rc); err != nil {
				t.Fatalf("%s: %v", f.Name, err) // includes a CRC mismatch
			}
			out.Close()
			rc.Close()
			os.Chmod(path, mode.Perm())
			os.Chtimes(path, f.Modified, f.Modified)
		}
	}
}

// tree describes a directory: path -> "dir mode", "file mode content" or
// "link target".
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			got[rel] = "dir " + info.Mode().Perm().String()
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			got[rel] = "link " + target
		default:
			b, _ := os.ReadFile(p)
			got[rel] = "file " + info.Mode().Perm().String() + " " + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: restored %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: restored but was never in the world", k)
		}
	}
}

// A test restore into a fresh directory recovers exactly the world as it was
// saved, for a live backup, a stopped one and a complete one.
func TestRestoreToFreshDirectory(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "stopped"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.create(t, "srv")
			w := world(t, filepath.Join(dir, "worldstorage"), "alpha")
			write(t, filepath.Join(w, "data", "scripts.sh"), "#!/bin/sh\n", 0o750)
			os.Symlink("level.dat", filepath.Join(w, "latest"))
			want := tree(t, w)
			e.con.running = live
			if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "restore")
			extract(t, filepath.Join(e.archives, "worlds", "srv", "alpha", stamp+".zip"), dest)
			sameTree(t, want, tree(t, filepath.Join(dest, "alpha")))
		})
	}
	t.Run("complete", func(t *testing.T) {
		e := newEnv(t, "")
		dir := e.create(t, "srv")
		world(t, filepath.Join(dir, "worldstorage"), "alpha")
		world(t, filepath.Join(dir, "worldstorage_inactive"), "gamma")
		write(t, filepath.Join(dir, "plugins", "p.yml"), "a: b\n", 0o640)
		// The marker and lock are not part of a server's data.
		if err := e.bm.Server(context.Background(), "srv", e.output()); err != nil {
			t.Fatal(err)
		}
		want := tree(t, dir)
		delete(want, ".msm.lock")
		dest := filepath.Join(t.TempDir(), "restore")
		extract(t, filepath.Join(e.archives, "backups", "srv", stamp+".zip"), dest)
		got := tree(t, filepath.Join(dest, "srv"))
		sameTree(t, want, got)
	})
}

var stampRE = regexp.MustCompile(`^(\d{4})[ \-_:](\d{1,2})[ \-_:](\d{1,2})(?:[ \-_:T](\d{1,2})[ \-_:](\d{1,2})[ \-_:](\d{1,2}))?`)

// worldEditSnapshots mirrors FileSystemSnapshotDatabase.getSnapshots from
// WorldEdit 7.3.x for a world directory inside the snapshot root: an entry
// named for the world, without level.dat, holding timestamped archives (or
// directories) that hold the world directory <world>/level.dat. The real
// source is run by TestWorldEditSnapshotAcceptance when its harness is built;
// this mirror runs everywhere.
func worldEditSnapshots(t *testing.T, root, world string) []string {
	t.Helper()
	var found []string
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != world || !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "level.dat")); err == nil {
			continue // a world directory, not a snapshot directory
		}
		subs, _ := os.ReadDir(filepath.Join(root, e.Name()))
		for _, s := range subs {
			if !stampRE.MatchString(s.Name()) || s.IsDir() {
				continue
			}
			r, err := zip.OpenReader(filepath.Join(root, e.Name(), s.Name()))
			if err != nil {
				continue // not an archive WorldEdit can open
			}
			for _, f := range r.File {
				if f.Name == world+"/level.dat" {
					found = append(found, s.Name())
					break
				}
			}
			r.Close()
		}
	}
	return found
}

// The archive layout is one WorldEdit's snapshot database lists: the
// snapshot directory is WORLD_ARCHIVE_PATH/<server>, and each world's
// timestamped archive holds <world>/level.dat.
func TestWorldEditSnapshotLayout(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(e.archives, "worlds", "srv")
	got := worldEditSnapshots(t, root, "alpha")
	if len(got) != 1 || got[0] != stamp+".zip" {
		t.Errorf("snapshots = %v", got)
	}
	// Control: a zip of the world's contents without the world folder is not
	// a snapshot, so the check can fail.
	other := filepath.Join(t.TempDir(), "snaps", "alpha")
	os.MkdirAll(other, 0o755)
	bad, _ := os.Create(filepath.Join(other, stamp+".zip"))
	zw := zip.NewWriter(bad)
	zw.Create("level.dat")
	zw.Close()
	bad.Close()
	if got := worldEditSnapshots(t, filepath.Dir(other), "alpha"); len(got) != 0 {
		t.Errorf("a rootless archive was accepted: %v", got)
	}
}

// Runs WorldEdit's own FileSystemSnapshotDatabase (see
// compatibility/worldedit/build.sh) over the archives and checks that it
// lists the snapshot and reads the world's level.dat from it. Set
// MSM_WORLDEDIT_HARNESS to the directory build.sh was given.
func TestWorldEditSnapshotAcceptance(t *testing.T) {
	harness := os.Getenv("MSM_WORLDEDIT_HARNESS")
	if harness == "" {
		t.Skip("MSM_WORLDEDIT_HARNESS is not set; build it with compatibility/worldedit/build.sh")
	}
	probe := func(root, world string) (string, error) {
		cmd := exec.Command("java", "-cp", strings.Join([]string{filepath.Join(harness, "classes"),
			filepath.Join(harness, "guava.jar"), filepath.Join(harness, "jsr305.jar")}, ":"), "Probe", root, world)
		out, err := cmd.Output()
		return string(out), err
	}
	e := newEnv(t, "")
	dir := e.create(t, "srv")
	world(t, filepath.Join(dir, "worldstorage"), "alpha")
	world(t, filepath.Join(dir, "worldstorage"), "beta")
	e.con.running = true
	if err := e.bm.Worlds(context.Background(), "srv", e.output()); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(e.archives, "worlds", "srv")
	for _, name := range []string{"alpha", "beta"} {
		out, err := probe(root, name)
		if err != nil {
			t.Fatalf("WorldEdit did not accept the %s snapshot: %v\n%s", name, err, out)
		}
		if !strings.Contains(out, "2026-01-02T03:04:05") || !strings.Contains(out, "level.dat=true") || !strings.Contains(out, "files=4") {
			t.Errorf("%s: unexpected WorldEdit answer:\n%s", name, out)
		}
	}
	// Control: WorldEdit rejects a rootless archive, so the probe can fail.
	bad := filepath.Join(t.TempDir(), "snaps")
	os.MkdirAll(filepath.Join(bad, "alpha"), 0o755)
	f, _ := os.Create(filepath.Join(bad, "alpha", stamp+".zip"))
	zw := zip.NewWriter(f)
	zw.Create("level.dat")
	zw.Close()
	f.Close()
	if out, err := probe(bad, "alpha"); err == nil {
		t.Errorf("WorldEdit accepted a rootless archive:\n%s", out)
	}
}
