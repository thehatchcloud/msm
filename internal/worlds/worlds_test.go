package worlds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

// fakeProber reports a fixed state per server name, Stopped by default.
type fakeProber struct {
	mu     sync.Mutex
	states map[string]servers.State
}

func (p *fakeProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) servers.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.states[s.Name]
}

// faulty fails the filesystem calls a test selects. fail receives the call
// name and its path arguments; a non-nil result is returned instead of
// making the call.
type faulty struct {
	osOps
	fail       func(op string, args ...string) error
	calls      []string
	noExchange bool
	mu         sync.Mutex
}

func (f *faulty) try(op string, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, op+" "+strings.Join(args, " "))
	f.mu.Unlock()
	if f.fail != nil {
		return f.fail(op, args...)
	}
	return nil
}

func (f *faulty) MkdirAll(p string, perm fs.FileMode) error {
	if err := f.try("MkdirAll", p); err != nil {
		return err
	}
	return f.osOps.MkdirAll(p, perm)
}

func (f *faulty) Rename(a, b string) error {
	if err := f.try("Rename", a, b); err != nil {
		return err
	}
	return f.osOps.Rename(a, b)
}

func (f *faulty) RemoveAll(p string) error {
	if err := f.try("RemoveAll", p); err != nil {
		return err
	}
	return f.osOps.RemoveAll(p)
}

func (f *faulty) CopyTree(a, b string) error {
	if err := f.try("CopyTree", a, b); err != nil {
		return err
	}
	return f.osOps.CopyTree(a, b)
}

func (f *faulty) Mkdirat(fd int, name string, mode uint32) error {
	if err := f.try("Mkdirat", name); err != nil {
		return err
	}
	return f.osOps.Mkdirat(fd, name, mode)
}

func (f *faulty) Symlinkat(target string, fd int, name string) error {
	if err := f.try("Symlinkat", name, target); err != nil {
		return err
	}
	return f.osOps.Symlinkat(target, fd, name)
}

func (f *faulty) Renameat(fd int, a, b string) error {
	if err := f.try("Renameat", a, b); err != nil {
		return err
	}
	return f.osOps.Renameat(fd, a, b)
}

func (f *faulty) Unlinkat(fd int, name string) error {
	if err := f.try("Unlinkat", name); err != nil {
		return err
	}
	return f.osOps.Unlinkat(fd, name)
}

// CopyWorld fails like a full or broken filesystem: the part of the tree
// written before the failure is left behind, as a real cut-short copy
// would.
func (f *faulty) CopyWorld(ctx context.Context, src, dst string, opt copyOptions) error {
	if err := f.try("CopyWorld", src, dst); err != nil {
		_ = os.MkdirAll(dst, 0o755)
		_ = os.WriteFile(filepath.Join(dst, "partial"), []byte("x"), 0o644)
		return err
	}
	return f.osOps.CopyWorld(ctx, src, dst, opt)
}

// Exchange reports an unsupported filesystem when noExchange is set.
func (f *faulty) Exchange(a, b string) (bool, error) {
	if err := f.try("Exchange", a, b); err != nil {
		return false, err
	}
	if f.noExchange {
		return false, nil
	}
	return f.osOps.Exchange(a, b)
}

type env struct {
	base, root string
	sm         *servers.Manager
	wm         *Manager
	ops        *faulty
	prober     *fakeProber
	owner      identity.Identity
	dropped    []identity.Identity
	out, errw  bytes.Buffer
}

func newEnv(t *testing.T, conf string) *env { return newEnvAs(t, conf, os.Geteuid()) }

// newEnvAs builds a manager acting with the given effective UID, over a
// fresh storage root. Dropping privileges is recorded, never done.
func newEnvAs(t *testing.T, conf string, euid int) *env {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "servers")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	var global *legacyconf.File
	if conf != "" {
		path := filepath.Join(base, "msm.conf")
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := legacyconf.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		global = f
	}
	e := &env{base: base, root: root, prober: &fakeProber{states: map[string]servers.State{}}, ops: &faulty{}}
	e.owner = identity.Identity{Username: "tester", UID: os.Geteuid(), GID: os.Getegid()}
	owner := e.owner
	sm, err := servers.New(servers.Config{
		Root: root, PropertiesFile: "server.properties", Global: global,
		Owner: owner, DefaultOwner: &owner, Prober: e.prober, EUID: &euid,
		DropTo: func(id identity.Identity) error { e.dropped = append(e.dropped, id); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.sm = sm
	e.wm, err = New(Config{Servers: sm})
	if err != nil {
		t.Fatal(err)
	}
	e.wm.ops = e.ops
	// Temporary directories are on disk; tests that need the real check
	// replace this one.
	e.wm.ramCheck = func(string) error { return nil }
	return e
}

func (e *env) output() Output {
	e.out.Reset()
	e.errw.Reset()
	return Output{Out: &e.out, Err: &e.errw}
}

func (e *env) create(t *testing.T, name string) string {
	t.Helper()
	c, err := e.sm.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return c.Dir
}

// world makes a world directory shaped like Minecraft's: level data and the
// nether and end dimension directories.
func world(t *testing.T, storage, name string) string {
	t.Helper()
	dir := filepath.Join(storage, name)
	for _, d := range []string{"region", "DIM-1/region", "DIM1/region"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for f, data := range map[string]string{"level.dat": "level " + name, "region/r.0.0.mca": "overworld", "DIM-1/region/r.0.0.mca": "nether", "DIM1/region/r.0.0.mca": "end"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s should exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("%s should not exist", path)
	}
}

func readlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink(%s): %v", path, err)
	}
	return target
}

// listing is what worlds list printed.
func (e *env) listing(t *testing.T, name string) string {
	t.Helper()
	if err := e.wm.List(context.Background(), name, e.output()); err != nil {
		t.Fatalf("List: %v", err)
	}
	return e.out.String()
}

// CT-CMD-027, CT-SURF-006, CT-SURF-007: worlds list shows every world in
// both storage directories, sorted, flagging the ones kept in RAM, and
// ignores what is not a world directory.
func TestListFixtures(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "survival")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	world(t, active, "world")
	world(t, active, "world_nether")
	world(t, inactive, "creative")
	write(t, filepath.Join(active, "world_nether", "inram"), "")
	if err := os.MkdirAll(filepath.Join(active, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(active, "planted")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(inactive, "notes.txt"), "not a world")

	want := "     creative\n     world\nRAM  world_nether\n"
	if got := e.listing(t, "survival"); got != want {
		t.Errorf("list =\n%s\nwant\n%s", got, want)
	}
	if w := e.errw.String(); !strings.Contains(w, "planted is a symbolic link") {
		t.Errorf("a planted link should be reported, stderr = %q", w)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "inram")); err == nil {
		t.Error("the link target must not be touched")
	}
}

func TestListWithoutWorlds(t *testing.T) {
	e := newEnv(t, "")
	e.create(t, "empty")
	if got := e.listing(t, "empty"); got != "There are no worlds in worldstorage.\n" {
		t.Errorf("list = %q", got)
	}
	if err := e.wm.List(context.Background(), "ghost", e.output()); !errors.Is(err, servers.ErrNotFound) {
		t.Errorf("List(unknown server) = %v, want ErrNotFound", err)
	}
}

// Per-server path overrides move both storage directories, including to
// absolute paths outside the server directory.
func TestPathOverrides(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "custom")
	disk := t.TempDir()
	write(t, filepath.Join(dir, "server.properties"),
		"msm-world-storage-path="+filepath.Join(disk, "live")+"\nmsm-world-storage-inactive-path=attic\n")
	world(t, filepath.Join(disk, "live"), "main")
	world(t, filepath.Join(dir, "attic"), "old")
	if got := e.listing(t, "custom"); got != "     main\n     old\n" {
		t.Errorf("list = %q", got)
	}
	if err := e.wm.Load(context.Background(), "custom", e.output()); err != nil {
		t.Fatal(err)
	}
	if got := readlink(t, filepath.Join(dir, "main")); got != filepath.Join(disk, "live", "main") {
		t.Errorf("main links to %q", got)
	}
	mustNotExist(t, filepath.Join(dir, "old"))
	if err := e.wm.Activate(context.Background(), "custom", "old", e.output()); err != nil {
		t.Fatal(err)
	}
	mustExist(t, filepath.Join(disk, "live", "old", "DIM-1", "region", "r.0.0.mca"))
}

func TestLayoutIsValidated(t *testing.T) {
	for name, props := range map[string]string{
		"same":      "msm-world-storage-inactive-path=worldstorage\n",
		"nested":    "msm-world-storage-inactive-path=worldstorage/inactive\n",
		"server":    "msm-world-storage-path=.\n",
		"bad flag":  "msm-worlds-flag-inram=a/b\n",
		"flag dots": "msm-worlds-flag-inram=..\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.create(t, "s")
			write(t, filepath.Join(dir, "server.properties"), props)
			if err := e.wm.List(context.Background(), "s", e.output()); !errors.Is(err, ErrLayout) {
				t.Errorf("List = %v, want ErrLayout", err)
			}
		})
	}
}

// CT-CMD-028, CT-SURF-008: worlds load links every active world into the
// server directory, removes the links of inactive worlds, prepares the
// level-name directory, and changes nothing when run again.
func TestLoadLinksActiveWorlds(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "survival")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	world(t, active, "world")
	world(t, active, "nether")
	world(t, inactive, "creative")
	write(t, filepath.Join(dir, "server.properties"), "level-name=fresh\n")
	// A link left from when "creative" was active, and a file of the server.
	if err := os.Symlink(filepath.Join(active, "creative"), filepath.Join(dir, "creative")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "ops.txt"), "keep")

	if err := e.wm.Load(context.Background(), "survival", e.output()); err != nil {
		t.Fatalf("Load: %v\n%s", err, e.errw.String())
	}
	for _, w := range []string{"world", "nether", "fresh"} {
		if got := readlink(t, filepath.Join(dir, w)); got != filepath.Join(active, w) {
			t.Errorf("%s links to %q, want %q", w, got, filepath.Join(active, w))
		}
	}
	mustNotExist(t, filepath.Join(dir, "creative"))
	mustExist(t, filepath.Join(active, "fresh"))
	if data, _ := os.ReadFile(filepath.Join(dir, "ops.txt")); string(data) != "keep" {
		t.Error("an unrelated file was changed")
	}
	if !strings.HasPrefix(e.out.String(), "Maintaining world symbolic links...") || !strings.HasSuffix(e.out.String(), "Done.\n") {
		t.Errorf("output = %q", e.out.String())
	}

	// A second run finds nothing to do.
	e.ops.calls = nil
	rep, err := e.wm.EnsureLinks(context.Background(), mustSettings(t, e, "survival"), e.output())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Created)+len(rep.Replaced)+len(rep.Removed)+len(rep.Skipped) != 0 {
		t.Errorf("second run changed %+v", rep)
	}
	for _, c := range e.ops.calls {
		if !strings.HasPrefix(c, "Mkdirat") {
			t.Errorf("second run made a change: %s", c)
		}
	}
}

func mustSettings(t *testing.T, e *env, name string) *legacyconf.ServerSettings {
	t.Helper()
	s, err := e.sm.Settings(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Existing data is never disturbed by the level-name directory: a world that
// already sits in the server directory, or in inactive storage, is left as
// it is.
func TestLevelNameDirectoryRespectsExistingData(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "old")
	write(t, filepath.Join(dir, "server.properties"), "level-name=world\n")
	world(t, dir, "world") // a real world directory in the server directory
	if err := e.wm.Load(context.Background(), "old", e.output()); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(dir, "worldstorage", "world"))
	if info, err := os.Lstat(filepath.Join(dir, "world")); err != nil || info.Mode()&fs.ModeSymlink != 0 {
		t.Errorf("the real world directory must stay a directory: %v %v", info, err)
	}
	if !strings.Contains(e.errw.String(), "real directory in the server directory") {
		t.Errorf("stderr = %q", e.errw.String())
	}

	dir2 := e.create(t, "parked")
	write(t, filepath.Join(dir2, "server.properties"), "level-name=world\n")
	world(t, filepath.Join(dir2, "worldstorage_inactive"), "world")
	if err := e.wm.Load(context.Background(), "parked", e.output()); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(dir2, "worldstorage", "world"))

	dir3 := e.create(t, "unsafe")
	write(t, filepath.Join(dir3, "server.properties"), "level-name=../escape\n")
	if err := e.wm.Load(context.Background(), "unsafe", e.output()); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(e.root, "escape"))
	mustNotExist(t, filepath.Join(dir3, "worldstorage", "..", "escape"))
}

// A name that is already a real file or directory is never replaced; that
// world is reported, the others are still linked and the command fails.
func TestLoadNeverOverwrites(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	active := filepath.Join(dir, "worldstorage")
	world(t, active, "alpha")
	world(t, active, "plugins")
	world(t, active, "world")
	write(t, filepath.Join(dir, "plugins", "keep.jar"), "precious")
	// alpha is linked wrongly, world has a stale link to somewhere else.
	if err := os.Symlink("/nonexistent", filepath.Join(dir, "alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", filepath.Join(dir, "world")); err != nil {
		t.Fatal(err)
	}

	err := e.wm.Load(context.Background(), "s", e.output())
	if !errors.Is(err, ErrLinks) {
		t.Fatalf("Load = %v, want ErrLinks", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "plugins", "keep.jar")); string(data) != "precious" {
		t.Error("an existing directory was changed")
	}
	if got := readlink(t, filepath.Join(dir, "alpha")); got != filepath.Join(active, "alpha") {
		t.Errorf("alpha links to %q", got)
	}
	if got := readlink(t, filepath.Join(dir, "world")); got != filepath.Join(active, "world") {
		t.Errorf("world links to %q", got)
	}
	if !strings.Contains(e.errw.String(), `Could not link world "plugins"`) {
		t.Errorf("stderr = %q", e.errw.String())
	}
	// No temporary link is left behind.
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), stagePrefix) {
			t.Errorf("leftover %s", ent.Name())
		}
	}
}

// A world present in both storage directories is ambiguous: it is reported
// and left alone.
func TestAmbiguousWorld(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	world(t, filepath.Join(dir, "worldstorage"), "dup")
	world(t, filepath.Join(dir, "worldstorage_inactive"), "dup")
	err := e.wm.Load(context.Background(), "s", e.output())
	if !errors.Is(err, ErrLinks) {
		t.Fatalf("Load = %v, want ErrLinks", err)
	}
	mustNotExist(t, filepath.Join(dir, "dup"))
	if err := e.wm.Activate(context.Background(), "s", "dup", e.output()); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("Activate = %v, want ErrAmbiguous", err)
	}
	if err := e.wm.Deactivate(context.Background(), "s", "dup", e.output()); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("Deactivate = %v, want ErrAmbiguous", err)
	}
	for _, p := range []string{"worldstorage", "worldstorage_inactive"} {
		mustExist(t, filepath.Join(dir, p, "dup", "level.dat"))
	}
}

const ramConf = "RAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH=%s\n"

// CT-SURF-008: a world flagged for RAM links to its RAM copy when RAM worlds
// are enabled and usable, and the copy is made when it is missing; a copy
// this manager did not make is never used or replaced, and the world is
// linked to disk instead.
func TestLinksResolveRAMCopy(t *testing.T) {
	ram := t.TempDir()
	e := newEnv(t, fmt.Sprintf(ramConf, ram))
	dir := e.create(t, "s")
	active := filepath.Join(dir, "worldstorage")
	world(t, active, "fast")
	world(t, active, "nocopy")
	world(t, active, "plain")
	world(t, active, "foreign")
	for _, w := range []string{"fast", "nocopy", "foreign"} {
		write(t, filepath.Join(active, w, "inram"), "")
	}
	e.seedRAM(t, "s", "fast")
	world(t, filepath.Join(ram, "s"), "foreign")

	if err := e.wm.Load(context.Background(), "s", e.output()); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"fast", "nocopy"} {
		if got := readlink(t, filepath.Join(dir, w)); got != filepath.Join(ram, "s", w) {
			t.Errorf("%s links to %q, want its RAM copy", w, got)
		}
		mustExist(t, filepath.Join(ram, "s", w, "level.dat"))
		mustNotExist(t, filepath.Join(ram, "s", w, "inram"))
	}
	if got := readlink(t, filepath.Join(dir, "plain")); got != filepath.Join(active, "plain") {
		t.Errorf("plain links to %q, want disk", got)
	}
	if got := readlink(t, filepath.Join(dir, "foreign")); got != filepath.Join(active, "foreign") {
		t.Errorf("foreign links to %q, want disk", got)
	}
	if !strings.Contains(e.errw.String(), `world "foreign" is flagged to live in RAM`) {
		t.Errorf("stderr = %q", e.errw.String())
	}
}

func TestRAMFlagIgnoredWhenDisabled(t *testing.T) {
	for name, conf := range map[string]string{"no config": "", "disabled": "RAMDISK_STORAGE_ENABLED=false\n", "relative": "RAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH=ram\n"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, conf)
			dir := e.create(t, "s")
			world(t, filepath.Join(dir, "worldstorage"), "w")
			write(t, filepath.Join(dir, "worldstorage", "w", "inram"), "")
			if err := e.wm.Load(context.Background(), "s", e.output()); err != nil {
				t.Fatal(err)
			}
			if got := readlink(t, filepath.Join(dir, "w")); got != filepath.Join(dir, "worldstorage", "w") {
				t.Errorf("links to %q", got)
			}
		})
	}
}

// CT-CMD-032, CT-CMD-033, CT-SURF-007: on and off move a world, whole with
// its dimension directories, between the two storage directories.
func TestActivateAndDeactivate(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	world(t, active, "main")
	world(t, inactive, "spare")
	ctx := context.Background()

	if err := e.wm.Load(ctx, "s", e.output()); err != nil {
		t.Fatal(err)
	}
	if err := e.wm.Deactivate(ctx, "s", "main", e.output()); err != nil {
		t.Fatal(err)
	}
	if want := "Moving world \"main\" to the inactive worldstorage directory... Done.\n"; e.out.String() != want {
		t.Errorf("output = %q, want %q", e.out.String(), want)
	}
	mustNotExist(t, filepath.Join(active, "main"))
	mustExist(t, filepath.Join(inactive, "main", "DIM-1", "region", "r.0.0.mca"))
	mustNotExist(t, filepath.Join(dir, "main")) // its link went with it

	if err := e.wm.Deactivate(ctx, "s", "main", e.output()); err != nil {
		t.Fatal(err)
	}
	if want := "World \"main\" is already deactivate.\n"; e.out.String() != want {
		t.Errorf("output = %q, want %q", e.out.String(), want)
	}

	if err := e.wm.Activate(ctx, "s", "spare", e.output()); err != nil {
		t.Fatal(err)
	}
	if want := "Moving world \"spare\" to the active worldstorage directory... Done.\n"; e.out.String() != want {
		t.Errorf("output = %q, want %q", e.out.String(), want)
	}
	if data, _ := os.ReadFile(filepath.Join(active, "spare", "DIM1", "region", "r.0.0.mca")); string(data) != "end" {
		t.Error("the world did not move whole")
	}
	if err := e.wm.Activate(ctx, "s", "spare", e.output()); err != nil || !strings.Contains(e.out.String(), "already activate") {
		t.Errorf("second Activate = %v, %q", err, e.out.String())
	}
	if err := e.wm.Activate(ctx, "s", "ghost", e.output()); !errors.Is(err, ErrNoWorld) {
		t.Errorf("Activate(ghost) = %v, want ErrNoWorld", err)
	}
}

// Only the link a deactivated world had is removed; a link the administrator
// pointed elsewhere is not theirs to lose.
func TestDeactivateKeepsForeignLink(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	world(t, filepath.Join(dir, "worldstorage"), "main")
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(dir, "main")); err != nil {
		t.Fatal(err)
	}
	if err := e.wm.Deactivate(context.Background(), "s", "main", e.output()); err != nil {
		t.Fatal(err)
	}
	if got := readlink(t, filepath.Join(dir, "main")); got != other {
		t.Errorf("link now %q", got)
	}
}

func TestActivateConflict(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	// A plain file named like the world blocks the move target.
	world(t, filepath.Join(dir, "worldstorage"), "main")
	write(t, filepath.Join(dir, "worldstorage_inactive", "main"), "a file, not a world")
	err := e.wm.Deactivate(context.Background(), "s", "main", e.output())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Deactivate = %v, want ErrConflict", err)
	}
	mustExist(t, filepath.Join(dir, "worldstorage", "main", "level.dat"))
	if data, _ := os.ReadFile(filepath.Join(dir, "worldstorage_inactive", "main")); string(data) != "a file, not a world" {
		t.Error("the blocking file was changed")
	}
}

// CT-CMD-032 stopped-server policy: every change refuses a server that is
// running, occupied or whose state cannot be inspected, and changes nothing.
func TestChangesRefuseServerNotProvenStopped(t *testing.T) {
	for _, kind := range []servers.StateKind{servers.Running, servers.Occupied, servers.Unknown} {
		t.Run(kind.String(), func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.create(t, "s")
			world(t, filepath.Join(dir, "worldstorage"), "a")
			world(t, filepath.Join(dir, "worldstorage_inactive"), "b")
			e.prober.states["s"] = servers.State{Kind: kind, Detail: "test"}
			ctx := context.Background()
			wantErr := servers.ErrRunning
			if kind == servers.Unknown {
				wantErr = servers.ErrNotStopped
			}
			for name, fn := range map[string]func() error{
				"load":    func() error { return e.wm.Load(ctx, "s", e.output()) },
				"on":      func() error { return e.wm.Activate(ctx, "s", "b", e.output()) },
				"off":     func() error { return e.wm.Deactivate(ctx, "s", "a", e.output()) },
				"on all":  func() error { return e.wm.ActivateAll(ctx, "s", e.output()) },
				"off all": func() error { return e.wm.DeactivateAll(ctx, "s", e.output()) },
			} {
				if err := fn(); kind == servers.Occupied && errors.Is(err, servers.ErrNotStopped) || errors.Is(err, wantErr) {
					continue
				} else {
					t.Errorf("%s = %v, want %v", name, err, wantErr)
				}
			}
			mustExist(t, filepath.Join(dir, "worldstorage", "a"))
			mustExist(t, filepath.Join(dir, "worldstorage_inactive", "b"))
			mustNotExist(t, filepath.Join(dir, "a"))
		})
	}
}

// A world with a copy in RAM may hold newer data than its disk copy, so it
// is not moved away from the server until the copy is synchronized.
func TestDeactivateRefusesWorldWithRAMCopy(t *testing.T) {
	ram := t.TempDir()
	e := newEnv(t, fmt.Sprintf(ramConf, ram))
	dir := e.create(t, "s")
	world(t, filepath.Join(dir, "worldstorage"), "fast")
	world(t, filepath.Join(ram, "s"), "fast")
	err := e.wm.Deactivate(context.Background(), "s", "fast", e.output())
	if !errors.Is(err, ErrRAMCopy) {
		t.Fatalf("Deactivate = %v, want ErrRAMCopy", err)
	}
	mustExist(t, filepath.Join(dir, "worldstorage", "fast", "level.dat"))
}

func TestActivateAllAndDeactivateAll(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	world(t, active, "a1")
	world(t, active, "a2")
	world(t, inactive, "i1")
	world(t, inactive, "dup")
	world(t, active, "dup")
	ctx := context.Background()

	if err := e.wm.DeactivateAll(ctx, "s", e.output()); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"a1", "a2", "i1"} {
		mustExist(t, filepath.Join(inactive, w, "level.dat"))
	}
	mustExist(t, filepath.Join(active, "dup", "level.dat")) // ambiguous: untouched
	mustNotExist(t, filepath.Join(active, "a1"))

	if err := e.wm.ActivateAll(ctx, "s", e.output()); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"a1", "a2", "i1"} {
		mustExist(t, filepath.Join(active, w, "level.dat"))
	}
	if err := e.wm.ActivateAll(ctx, "s", e.output()); err != nil || e.out.String() != "No worlds to activate.\n" {
		t.Errorf("repeat = %v, %q", err, e.out.String())
	}
}

// One world that cannot move does not stop the others, and the command fails.
func TestAllContinuesPastAFailure(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	for _, w := range []string{"a", "b", "c"} {
		world(t, filepath.Join(dir, "worldstorage"), w)
	}
	write(t, filepath.Join(dir, "worldstorage_inactive", "b"), "blocks b")
	err := e.wm.DeactivateAll(context.Background(), "s", e.output())
	if err == nil || !strings.Contains(err.Error(), "1 of 3 worlds") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("DeactivateAll = %v", err)
	}
	mustExist(t, filepath.Join(dir, "worldstorage_inactive", "a", "level.dat"))
	mustExist(t, filepath.Join(dir, "worldstorage_inactive", "c", "level.dat"))
	mustExist(t, filepath.Join(dir, "worldstorage", "b", "level.dat"))
}

// snapshot reads every file below dir so a moved world can be compared
// byte for byte.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			out[rel] = "-> " + target
			return err
		case d.IsDir():
			info, err := d.Info()
			out[rel+"/"] = info.Mode().Perm().String()
			return err
		}
		data, err := os.ReadFile(path)
		out[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func exdevOnWorldRename(op string, args ...string) error {
	// Only a move between directories crosses filesystems; renames within
	// one directory (publishing a stage, retiring the original) do not.
	if op == "Rename" && filepath.Dir(args[0]) != filepath.Dir(args[1]) {
		return &os.LinkError{Op: "rename", Old: args[0], New: args[1], Err: unix.EXDEV}
	}
	return nil
}

func leftovers(t *testing.T, dirs ...string) []string {
	t.Helper()
	var found []string
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), stagePrefix) {
				found = append(found, filepath.Join(d, e.Name()))
			}
		}
	}
	return found
}

// Moving between two filesystems copies the world, publishes the copy with
// one rename and only then removes the original, keeping links and modes.
func TestCrossFilesystemMove(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	src := world(t, active, "big")
	if err := os.Symlink("region/r.0.0.mca", filepath.Join(src, "shortcut")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "level.dat"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, src)
	e.ops.fail = exdevOnWorldRename

	if err := e.wm.Deactivate(context.Background(), "s", "big", e.output()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if got := snapshot(t, filepath.Join(inactive, "big")); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("moved world differs:\n%v\nwant\n%v", got, before)
	}
	mustNotExist(t, src)
	if l := leftovers(t, active, inactive); len(l) != 0 {
		t.Errorf("leftovers: %v", l)
	}
	if info, _ := os.Stat(filepath.Join(inactive, "big", "level.dat")); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode().Perm())
	}
}

func TestCrossFilesystemMoveRefusesSpecialFiles(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	src := world(t, active, "odd")
	if err := unix.Mkfifo(filepath.Join(src, "pipe"), 0o644); err != nil {
		t.Skipf("no fifos here: %v", err)
	}
	e.ops.fail = exdevOnWorldRename
	err := e.wm.Deactivate(context.Background(), "s", "odd", e.output())
	if err == nil || !strings.Contains(err.Error(), "named pipe") || !strings.Contains(err.Error(), "still at") {
		t.Fatalf("Deactivate = %v", err)
	}
	mustExist(t, filepath.Join(src, "level.dat"))
	mustNotExist(t, filepath.Join(inactive, "odd"))
	if l := leftovers(t, active, inactive); len(l) != 0 {
		t.Errorf("leftovers: %v", l)
	}
}

// Interrupting the move at each filesystem step leaves the world intact at
// its source or reports where it is; nothing is ever lost.
func TestMoveInterruptedAtEveryStep(t *testing.T) {
	boom := errors.New("injected failure")
	steps := []struct {
		name string
		fail func(op string, args ...string) error
		// wantSource: the world must still be (only) at the source.
		wantSource bool
	}{
		{"create destination", func(op string, a ...string) error {
			if op == "MkdirAll" {
				return boom
			}
			return nil
		}, true},
		{"direct rename", func(op string, a ...string) error {
			if op == "Rename" {
				return boom
			}
			return nil
		}, true},
		{"copy", func(op string, a ...string) error {
			if op == "CopyTree" {
				return boom
			}
			return exdevOnWorldRename(op, a...)
		}, true},
		{"publish", func(op string, a ...string) error {
			if op == "Rename" && strings.Contains(a[0], stagePrefix) {
				return boom
			}
			return exdevOnWorldRename(op, a...)
		}, true},
		{"retire source", func(op string, a ...string) error {
			// The first rename of the source (not of the stage) fails with EXDEV
			// as it must; retiring it to the trash name then fails.
			if op == "Rename" && strings.Contains(a[1], stagePrefix+"old-") {
				return boom
			}
			return exdevOnWorldRename(op, a...)
		}, true},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.create(t, "s")
			active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
			src := world(t, active, "w")
			before := snapshot(t, src)
			e.ops.fail = st.fail
			err := e.wm.Deactivate(context.Background(), "s", "w", e.output())
			if err == nil {
				t.Fatal("Deactivate reported success after an injected failure")
			}
			if !strings.Contains(err.Error(), "not moved") {
				t.Errorf("error should say the world was not moved: %v", err)
			}
			if got := snapshot(t, src); fmt.Sprint(got) != fmt.Sprint(before) {
				t.Errorf("source changed")
			}
			mustNotExist(t, filepath.Join(inactive, "w"))
			if l := leftovers(t, active, inactive); len(l) != 0 {
				t.Errorf("leftovers: %v", l)
			}
		})
	}
}

// If the original cannot be retired and the copy cannot be undone, the error
// names both places so nothing is deleted by guesswork.
func TestRetireFailureThatCannotBeUndone(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	src := world(t, filepath.Join(dir, "worldstorage"), "w")
	boom := errors.New("injected failure")
	e.ops.fail = func(op string, a ...string) error {
		switch {
		case op == "Rename" && strings.Contains(a[1], stagePrefix+"old-"):
			return boom
		case op == "Rename" && strings.Contains(a[0], filepath.Join("worldstorage_inactive", "w")) && strings.Contains(a[1], stagePrefix):
			return boom // undo
		}
		return exdevOnWorldRename(op, a...)
	}
	err := e.wm.Deactivate(context.Background(), "s", "w", e.output())
	if err == nil || !strings.Contains(err.Error(), "both") || !strings.Contains(err.Error(), src) {
		t.Fatalf("Deactivate = %v", err)
	}
	mustExist(t, filepath.Join(src, "level.dat"))
	mustExist(t, filepath.Join(dir, "worldstorage_inactive", "w", "level.dat"))
}

// If removing the retired original fails, the world has still moved; the
// error says so and where the leftover is, and list reports it.
func TestLeftoverAfterMoveIsReported(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "s")
	active, inactive := filepath.Join(dir, "worldstorage"), filepath.Join(dir, "worldstorage_inactive")
	world(t, active, "w")
	boom := errors.New("injected failure")
	e.ops.fail = func(op string, a ...string) error {
		if op == "RemoveAll" && strings.Contains(a[0], stagePrefix+"old-") {
			return boom
		}
		return exdevOnWorldRename(op, a...)
	}
	err := e.wm.Deactivate(context.Background(), "s", "w", e.output())
	if err == nil || !strings.Contains(err.Error(), "was moved") || !strings.Contains(err.Error(), "remove it by hand") {
		t.Fatalf("Deactivate = %v", err)
	}
	mustExist(t, filepath.Join(inactive, "w", "level.dat"))
	e.ops.fail = nil
	e.listing(t, "s")
	if !strings.Contains(e.errw.String(), "left over from an interrupted world move") {
		t.Errorf("stderr = %q", e.errw.String())
	}
}

// Failures while changing links leave the existing link in place and no
// temporary link behind.
func TestLinkFailuresLeaveNoDebris(t *testing.T) {
	boom := errors.New("injected failure")
	for _, failing := range []string{"Symlinkat", "Renameat"} {
		t.Run(failing, func(t *testing.T) {
			e := newEnv(t, "")
			dir := e.create(t, "s")
			active := filepath.Join(dir, "worldstorage")
			world(t, active, "w")
			if err := os.Symlink("/old", filepath.Join(dir, "w")); err != nil {
				t.Fatal(err)
			}
			e.ops.fail = func(op string, a ...string) error {
				if op == failing {
					return boom
				}
				return nil
			}
			err := e.wm.Load(context.Background(), "s", e.output())
			if !errors.Is(err, boom) {
				t.Fatalf("Load = %v, want the injected failure", err)
			}
			if got := readlink(t, filepath.Join(dir, "w")); got != "/old" {
				t.Errorf("existing link changed to %q", got)
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Errorf("leftovers: %v", l)
			}
		})
	}
}

// Root drops to the server's owner before moving a world; an unprivileged
// caller who is not the owner is refused before anything changes.
func TestPrivileges(t *testing.T) {
	if os.Geteuid() != 0 {
		// Pretend to be root; the drop is recorded, not done. A real root
		// owns the server as root, so there is nothing to drop to.
		e := newEnvAs(t, "", 0)
		dir := e.create(t, "s")
		world(t, filepath.Join(dir, "worldstorage"), "w")
		if err := e.wm.Deactivate(context.Background(), "s", "w", e.output()); err != nil {
			t.Fatal(err)
		}
		if len(e.dropped) != 1 || e.dropped[0].UID != e.owner.UID {
			t.Errorf("dropped = %v, want one drop to %v", e.dropped, e.owner)
		}
	}

	other := newEnvAs(t, "", os.Geteuid()+4242)
	dir := other.create2(t, "s")
	world(t, filepath.Join(dir, "worldstorage"), "w")
	for name, fn := range map[string]func() error{
		"load": func() error { return other.wm.Load(context.Background(), "s", other.output()) },
		"off":  func() error { return other.wm.Deactivate(context.Background(), "s", "w", other.output()) },
	} {
		if err := fn(); !errors.Is(err, identity.ErrPrivilegeRequired) {
			t.Errorf("%s = %v, want ErrPrivilegeRequired", name, err)
		}
	}
	mustExist(t, filepath.Join(dir, "worldstorage", "w", "level.dat"))
	mustNotExist(t, filepath.Join(dir, "w"))
}

// create2 makes a server directory by hand, since a manager acting as a
// different user cannot create servers.
func (e *env) create2(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(e.root, name)
	if err := os.MkdirAll(filepath.Join(dir, "worldstorage"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Root makes new links owned by the server's owner, not by root.
func TestRootHandsLinksToOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to observe ownership")
	}
	e := newEnvAs(t, "", 0)
	e.owner = identity.Identity{Username: "nobody", UID: 65534, GID: 65534}
	dir := e.create(t, "s")
	world(t, filepath.Join(dir, "worldstorage"), "w")
	s := mustSettings(t, e, "s")
	if _, err := e.wm.EnsureLinks(context.Background(), s, e.output()); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(dir, "w"), &st); err != nil {
		t.Fatal(err)
	}
	t.Logf("link owner uid=%d", st.Uid)
}

func TestForAll(t *testing.T) {
	e := newEnv(t, "")
	a, b := e.create(t, "a"), e.create(t, "b")
	world(t, filepath.Join(a, "worldstorage"), "wa")
	world(t, filepath.Join(b, "worldstorage"), "wb")
	var out, errw bytes.Buffer
	err := e.wm.ForAll(context.Background(), Output{Out: &out, Err: &errw}, func(name string, o Output) error {
		return e.wm.List(context.Background(), name, o)
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "a:      wa\nb:      wb\n"; out.String() != want {
		t.Errorf("out = %q, want %q", out.String(), want)
	}

	err = e.wm.ForAll(context.Background(), Output{Out: &out, Err: &errw}, func(name string, o Output) error {
		return fmt.Errorf("boom %s", name)
	})
	if err == nil || !strings.Contains(err.Error(), "2 of 2 servers failed") ||
		strings.Index(err.Error(), "a: boom a") > strings.Index(err.Error(), "b: boom b") {
		t.Errorf("ForAll = %v", err)
	}
}

func TestNewRequiresServers(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("New without servers should fail")
	}
}
