package servers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
)

// fakeProber reports a fixed state per server name, Stopped by default.
type fakeProber struct {
	mu     sync.Mutex
	states map[string]State
	calls  []string
}

func (p *fakeProber) Probe(_ context.Context, s *legacyconf.ServerSettings, _ identity.Identity) State {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, s.Name)
	return p.states[s.Name]
}

type env struct {
	root    string
	mgr     *Manager
	prober  *fakeProber
	owner   identity.Identity
	dropped []identity.Identity
}

// newEnv builds a manager over a fresh storage root, acting as the
// invoking user, so no test changes real ownership or privileges.
func newEnv(t *testing.T, conf string) *env {
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
	e := &env{root: root, prober: &fakeProber{states: map[string]State{}}}
	e.owner = identity.Identity{Username: "tester", UID: os.Geteuid(), GID: os.Getegid()}
	e.mgr = e.manager(t, global, e.owner.UID)
	return e
}

func (e *env) manager(t *testing.T, global *legacyconf.File, euid int) *Manager {
	t.Helper()
	owner := e.owner
	m, err := New(Config{
		Root: e.root, PropertiesFile: "server.properties", Global: global,
		Owner: owner, DefaultOwner: &owner, Prober: e.prober, EUID: &euid,
		DropTo: func(id identity.Identity) error { e.dropped = append(e.dropped, id); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) create(t *testing.T, name string) string {
	t.Helper()
	c, err := e.mgr.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return c.Dir
}

// tree lists every path below dir with file contents and link targets,
// ignoring lock files, so two trees can be compared exactly.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir || d.Name() == ".msm.lock" {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			out[rel] = "-> " + target
			return err
		case d.IsDir():
			out[rel+"/"] = ""
		default:
			data, err := os.ReadFile(path)
			out[rel] = string(data)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func keys(m map[string]string) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

// CT-CMD-010: server create <name> makes the legacy layout.
func TestCreateLayout(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "survival")
	want := map[string]string{
		"whitelist.json":          "[]\n",
		"banned-ips.json":         "[]\n",
		"banned-players.json":     "[]\n",
		"ops.json":                "[]\n",
		"server.properties":       "",
		"worldstorage/":           "",
		"worldstorage/readme.txt": worldStorageReadme,
	}
	got := tree(t, dir)
	if fmt.Sprint(keys(got)) != fmt.Sprint(keys(want)) {
		t.Fatalf("layout = %v, want %v", keys(got), keys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "eula.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("create must never write eula.txt: accepting the EULA is explicit")
	}
	if _, err := os.Stat(filepath.Join(dir, "active")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a new server is not marked active")
	}
	if len(e.dropped) != 0 {
		t.Fatal("an unprivileged create must not drop privileges")
	}
}

func TestCreateIsDeterministic(t *testing.T) {
	e := newEnv(t, "")
	a, b := tree(t, e.create(t, "one")), tree(t, e.create(t, "two"))
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("two fresh servers differ:\n%v\n%v", a, b)
	}
}

func TestCreateHonoursMsmConfDefaults(t *testing.T) {
	e := newEnv(t, `DEFAULT_OPS_LIST=" alice, bob ,,"
DEFAULT_WORLD_STORAGE_PATH="worlds/storage"
DEFAULT_WHITELIST_PATH="allow.json"
DEFAULT_BANNED_IPS_PATH="/etc/msm-should-not-write.json"
`)
	c, err := e.mgr.Create(context.Background(), "custom")
	if err != nil {
		t.Fatal(err)
	}
	got := tree(t, c.Dir)
	if got["ops.txt"] != "alice\nbob\n" {
		t.Errorf("ops.txt = %q", got["ops.txt"])
	}
	if got["allow.json"] != "[]\n" || got["worlds/storage/readme.txt"] != worldStorageReadme {
		t.Errorf("configured paths not used: %v", keys(got))
	}
	if _, ok := got["banned-ips.json"]; ok {
		t.Error("an absolute DEFAULT_BANNED_IPS_PATH must not fall back to the built-in name")
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "DEFAULT_BANNED_IPS_PATH") {
		t.Fatalf("warnings = %q", c.Warnings)
	}
	if _, err := os.Stat("/etc/msm-should-not-write.json"); err == nil {
		t.Fatal("create wrote outside the instance")
	}
}

func TestCreateRefusesUnsafeNames(t *testing.T) {
	e := newEnv(t, "")
	e.create(t, "Survival")
	if err := os.Symlink(t.TempDir(), filepath.Join(e.root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, "plainfile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		want error
	}{
		{"", ErrInvalidName},
		{"has space", ErrInvalidName},
		{"../escape", ErrInvalidName},
		{"a/b", ErrInvalidName},
		{".hidden", ErrInvalidName},
		{"--noinput", ErrInvalidName},
		{"all", ErrInvalidName},
		{"Server", ErrInvalidName},
		{"$(touch x)", ErrInvalidName},
		{"Survival", ErrExists},
		{"survival", ErrCaseCollision},
		{"linked", ErrExists},
		{"plainfile", ErrExists},
	}
	for _, tc := range cases {
		if _, err := e.mgr.Create(context.Background(), tc.name); !errors.Is(err, tc.want) {
			t.Errorf("Create(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}
	entries, _ := os.ReadDir(e.root)
	var names []string
	for _, d := range entries {
		names = append(names, d.Name())
	}
	if fmt.Sprint(names) != "[.msm-servers.lock Survival linked plainfile]" {
		t.Fatalf("storage root changed: %v", names)
	}
}

func TestCreateRequiresStorageRoot(t *testing.T) {
	e := newEnv(t, "")
	if err := os.Remove(e.root); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.Create(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Create without a storage root = %v", err)
	}
}

// failOps fails exactly the n-th mutation (1-based) and passes the rest
// through, so every step of an operation can be interrupted in turn.
type failOps struct {
	fsOps
	n, calls int
	log      []string
}

var errInjected = errors.New("injected failure")

func (f *failOps) step(desc string) error {
	f.calls++
	f.log = append(f.log, desc)
	if f.calls == f.n {
		return fmt.Errorf("%s: %w", desc, errInjected)
	}
	return nil
}

func (f *failOps) Mkdir(p string, m fs.FileMode) error {
	if err := f.step("mkdir " + filepath.Base(p)); err != nil {
		return err
	}
	return f.fsOps.Mkdir(p, m)
}

func (f *failOps) WriteFile(p string, d []byte, m fs.FileMode) error {
	if err := f.step("write " + filepath.Base(p)); err != nil {
		return err
	}
	return f.fsOps.WriteFile(p, d, m)
}

func (f *failOps) Rename(a, b string) error {
	if err := f.step("rename " + filepath.Base(a) + " " + filepath.Base(b)); err != nil {
		return err
	}
	return f.fsOps.Rename(a, b)
}

func (f *failOps) RemoveAll(p string) error {
	if err := f.step("removeall " + filepath.Base(p)); err != nil {
		return err
	}
	return f.fsOps.RemoveAll(p)
}

func (f *failOps) Symlink(target, l string) error {
	if err := f.step("symlink " + filepath.Base(l)); err != nil {
		return err
	}
	return f.fsOps.Symlink(target, l)
}

// countSteps runs op once without failures and reports how many mutations
// it made.
func countSteps(t *testing.T, e *env, op func() error) int {
	t.Helper()
	f := &failOps{fsOps: osOps{}}
	e.mgr.ops = f
	if err := op(); err != nil {
		t.Fatal(err)
	}
	e.mgr.ops = osOps{}
	return f.calls
}

func TestCreateInterruptedAtEveryStep(t *testing.T) {
	probe := newEnv(t, `DEFAULT_OPS_LIST="alice"`)
	steps := countSteps(t, probe, func() error { _, err := probe.mgr.Create(context.Background(), "x"); return err })
	if steps < 8 {
		t.Fatalf("only %d steps observed", steps)
	}
	for n := 1; n <= steps; n++ {
		e := newEnv(t, `DEFAULT_OPS_LIST="alice"`)
		f := &failOps{fsOps: osOps{}, n: n}
		e.mgr.ops = f
		_, err := e.mgr.Create(context.Background(), "x")
		if !errors.Is(err, errInjected) {
			t.Fatalf("step %d (%s): err = %v", n, f.log[n-1], err)
		}
		if _, err := os.Lstat(filepath.Join(e.root, "x")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("step %d (%s): a failed create published the server", n, f.log[n-1])
		}
		if list, warnings, _ := e.mgr.List(context.Background()); len(list) != 0 || len(warnings) != 0 {
			t.Fatalf("step %d (%s): leftovers %v %v", n, f.log[n-1], list, warnings)
		}
	}
}

func TestCreateCleanupFailureIsReported(t *testing.T) {
	e := newEnv(t, "")
	// Fail the readme write, then the staging cleanup.
	f := &failOps{fsOps: osOps{}, n: 7}
	e.mgr.ops = &failTwice{failOps: f, second: 8}
	_, err := e.mgr.Create(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "removed by hand") {
		t.Fatalf("err = %v", err)
	}
	_, warnings, _ := e.mgr.List(context.Background())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "interrupted server create") {
		t.Fatalf("warnings = %q", warnings)
	}
}

// failTwice also fails a second, later step.
type failTwice struct {
	*failOps
	second int
}

func (f *failTwice) RemoveAll(p string) error {
	if f.calls+1 == f.second {
		f.calls++
		return errInjected
	}
	return f.failOps.RemoveAll(p)
}

// CT-CMD-009: server list reports intent and liveness per server.
func TestList(t *testing.T) {
	e := newEnv(t, "")
	for _, n := range []string{"b-up", "a-down", "c-rogue", "d-unknown"} {
		e.create(t, n)
	}
	for _, n := range []string{"b-up", "a-down"} {
		if err := os.WriteFile(filepath.Join(e.root, n, "active"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.prober.states["b-up"] = State{Kind: Running}
	e.prober.states["c-rogue"] = State{Kind: Running}
	e.prober.states["d-unknown"] = State{Kind: Unknown, Detail: "no access"}
	for _, junk := range []string{".msm-create-x-1", ".msm-delete-y-2", "bad name", "start"} {
		if err := os.Mkdir(filepath.Join(e.root, junk), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(e.root, "a-down"), filepath.Join(e.root, "alias")); err != nil {
		t.Fatal(err)
	}
	list, warnings, err := e.mgr.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range list {
		got = append(got, fmt.Sprintf("%s:%v:%s", s.Name, s.Active, s.State.Kind))
	}
	if want := "[a-down:true:stopped b-up:true:running c-rogue:false:running d-unknown:false:unknown]"; fmt.Sprint(got) != want {
		t.Fatalf("list = %v, want %s", got, want)
	}
	if len(warnings) != 5 {
		t.Fatalf("warnings = %q", warnings)
	}
}

func TestListMissingRoot(t *testing.T) {
	e := newEnv(t, "")
	os.Remove(e.root)
	list, warnings, err := e.mgr.List(context.Background())
	if err != nil || list != nil || warnings != nil {
		t.Fatalf("List = %v %v %v", list, warnings, err)
	}
}

// addWorldLinks gives a server what the legacy manager's world links look
// like: an absolute link into its own world storage, plus a JAR link into
// the shared store.
func addWorldLinks(t *testing.T, dir, jarStore string) {
	t.Helper()
	world := filepath.Join(dir, "worldstorage", "world")
	if err := os.MkdirAll(world, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(world, "level.dat"), []byte("level"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(world, filepath.Join(dir, "world")); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(jarStore, "minecraft", "server.jar")
	if err := os.MkdirAll(filepath.Dir(jar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jar, []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(jar, filepath.Join(dir, "server.jar")); err != nil {
		t.Fatal(err)
	}
}

// CT-CMD-012: server rename <name> <name> moves a stopped server and keeps
// its world links working.
func TestRename(t *testing.T) {
	e := newEnv(t, "")
	jars := t.TempDir()
	addWorldLinks(t, e.create(t, "old"), jars)
	r, err := e.mgr.Rename(context.Background(), "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(e.root, "old")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("old directory still exists")
	}
	world, _ := os.Readlink(filepath.Join(e.root, "new", "world"))
	if world != filepath.Join(e.root, "new", "worldstorage", "world") {
		t.Fatalf("world link = %s", world)
	}
	if data, err := os.ReadFile(filepath.Join(e.root, "new", "world", "level.dat")); err != nil || string(data) != "level" {
		t.Fatalf("world not reachable through link: %v", err)
	}
	jar, _ := os.Readlink(filepath.Join(e.root, "new", "server.jar"))
	if jar != filepath.Join(jars, "minecraft", "server.jar") {
		t.Fatalf("JAR link changed to %s", jar)
	}
	if len(r.Links) != 1 || len(r.Notes) == 0 {
		t.Fatalf("result = %+v", r)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.root, "new")); len(entries) == 0 {
		t.Fatal("empty")
	} else {
		for _, d := range entries {
			if strings.HasPrefix(d.Name(), ".msm-link-") {
				t.Fatalf("temporary link left behind: %s", d.Name())
			}
		}
	}
}

func TestRenameRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.create(t, "old")
	e.create(t, "taken")
	e.create(t, "Taken2")
	cases := []struct {
		from, to string
		state    StateKind
		want     error
	}{
		{"old", "new", Running, ErrRunning},
		{"old", "new", Occupied, ErrNotStopped},
		{"old", "new", Unknown, ErrNotStopped},
		{"old", "taken", Stopped, ErrExists},
		{"old", "taken2", Stopped, ErrCaseCollision},
		{"old", "OLD", Stopped, ErrCaseCollision},
		{"old", "all", Stopped, ErrInvalidName},
		{"old", "../x", Stopped, ErrInvalidName},
		{"missing", "new", Stopped, ErrNotFound},
		{"OLD", "new", Stopped, ErrNotFound},
	}
	for _, tc := range cases {
		e.prober.states["old"] = State{Kind: tc.state, Detail: "test"}
		if _, err := e.mgr.Rename(context.Background(), tc.from, tc.to); !errors.Is(err, tc.want) {
			t.Errorf("Rename(%q, %q) with %s = %v, want %v", tc.from, tc.to, tc.state, err, tc.want)
		}
		if _, err := os.Stat(filepath.Join(e.root, "old", "server.properties")); err != nil {
			t.Fatalf("Rename(%q, %q) disturbed the server: %v", tc.from, tc.to, err)
		}
	}
}

func TestRenameRefusesSymlinkedServer(t *testing.T) {
	e := newEnv(t, "")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.mgr.Rename(context.Background(), "linked", "new"); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("err = %v", err)
	}
}

func TestRenameInterruptedAtEveryStep(t *testing.T) {
	setup := func(t *testing.T) *env {
		e := newEnv(t, "")
		dir := e.create(t, "old")
		addWorldLinks(t, dir, filepath.Join(filepath.Dir(e.root), "jars"))
		// A second world link, so a failure can land between two links.
		nether := filepath.Join(dir, "worldstorage", "nether")
		os.MkdirAll(nether, 0o755)
		os.Symlink(nether, filepath.Join(dir, "nether"))
		return e
	}
	probe := setup(t)
	before := tree(t, filepath.Join(probe.root, "old"))
	steps := countSteps(t, probe, func() error { _, err := probe.mgr.Rename(context.Background(), "old", "new"); return err })
	if steps != 5 {
		t.Fatalf("rename made %d mutations, want 5 (rename + 2 x (symlink + rename))", steps)
	}
	for n := 1; n <= steps; n++ {
		e := setup(t)
		f := &failOps{fsOps: osOps{}, n: n}
		e.mgr.ops = f
		_, err := e.mgr.Rename(context.Background(), "old", "new")
		if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), "nothing was renamed") {
			t.Fatalf("step %d (%s): err = %v", n, f.log[n-1], err)
		}
		if _, err := os.Lstat(filepath.Join(e.root, "new")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("step %d: new name exists after a failed rename", n)
		}
		// Same layout and link targets as before, relative to the root.
		after := tree(t, filepath.Join(e.root, "old"))
		for k, v := range before {
			v = strings.ReplaceAll(v, filepath.Dir(probe.root), filepath.Dir(e.root))
			if after[k] != v {
				t.Fatalf("step %d: %s = %q, want %q", n, k, after[k], v)
			}
		}
		if len(after) != len(before) {
			t.Fatalf("step %d: leftovers %v", n, keys(after))
		}
	}
}

func TestRenameRollbackFailureIsReported(t *testing.T) {
	e := newEnv(t, "")
	addWorldLinks(t, e.create(t, "old"), t.TempDir())
	// Fail the link symlink (step 2) and then the move back (step 3).
	f := &failOps{fsOps: osOps{}, n: 2}
	e.mgr.ops = &failRenameAt{failOps: f, at: 3}
	_, err := e.mgr.Rename(context.Background(), "old", "new")
	if err == nil || !strings.Contains(err.Error(), "rollback failed") || !strings.Contains(err.Error(), filepath.Join(e.root, "new")) {
		t.Fatalf("err = %v", err)
	}
}

type failRenameAt struct {
	*failOps
	at int
}

func (f *failRenameAt) Rename(a, b string) error {
	if f.calls+1 == f.at {
		f.calls++
		return errInjected
	}
	return f.failOps.Rename(a, b)
}

// CT-CMD-011: server delete <name> previews, then removes only the
// instance.
func TestDelete(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "doomed")
	jars := t.TempDir()
	addWorldLinks(t, dir, jars)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "worldstorage", "external"))

	p, err := e.mgr.PlanDelete(context.Background(), "doomed")
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != dir || p.State.Kind != Stopped || p.Files < 7 || len(p.ExternalLinks) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	for _, name := range p.Entries {
		if name == ".msm.lock" {
			t.Fatal("the preview lists the manager's lock file")
		}
	}
	if err := e.mgr.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("server still exists")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(data) != "keep" {
		t.Fatal("delete followed a symbolic link out of the server")
	}
	if _, err := os.Stat(filepath.Join(jars, "minecraft", "server.jar")); err != nil {
		t.Fatal("delete removed the shared JAR")
	}
	if list, warnings, _ := e.mgr.List(context.Background()); len(list) != 0 || len(warnings) != 0 {
		t.Fatalf("leftovers: %v %v", list, warnings)
	}
}

func TestDeleteRefusals(t *testing.T) {
	e := newEnv(t, "")
	e.create(t, "doomed")
	for _, kind := range []StateKind{Running, Occupied, Unknown} {
		p, err := e.mgr.PlanDelete(context.Background(), "doomed")
		if err != nil {
			t.Fatal(err)
		}
		e.prober.states["doomed"] = State{Kind: kind}
		if err := e.mgr.Delete(context.Background(), p); err == nil {
			t.Fatalf("deleted a %s server", kind)
		}
		e.prober.states["doomed"] = State{}
	}
	// The directory is replaced between preview and confirmation.
	p, err := e.mgr.PlanDelete(context.Background(), "doomed")
	if err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(e.root, "doomed"), filepath.Join(e.root, "elsewhere"))
	os.Mkdir(filepath.Join(e.root, "doomed"), 0o755)
	if err := e.mgr.Delete(context.Background(), p); !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "elsewhere", "server.properties")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "all", "../x"} {
		if _, err := e.mgr.PlanDelete(context.Background(), name); err == nil {
			t.Errorf("PlanDelete(%q) succeeded", name)
		}
	}
}

func TestDeleteInterrupted(t *testing.T) {
	// Step 1 moves the server out of service; step 2 removes it.
	for n, want := range map[int]error{1: errInjected, 2: ErrCleanup} {
		e := newEnv(t, "")
		dir := e.create(t, "doomed")
		p, err := e.mgr.PlanDelete(context.Background(), "doomed")
		if err != nil {
			t.Fatal(err)
		}
		e.mgr.ops = &failOps{fsOps: osOps{}, n: n}
		err = e.mgr.Delete(context.Background(), p)
		if !errors.Is(err, want) {
			t.Fatalf("step %d: err = %v, want %v", n, err, want)
		}
		list, warnings, _ := e.mgr.List(context.Background())
		switch n {
		case 1:
			if _, err := os.Stat(filepath.Join(dir, "server.properties")); err != nil || len(list) != 1 || len(warnings) != 0 {
				t.Fatalf("a failed delete disturbed the server: %v %v", list, warnings)
			}
		case 2:
			if len(list) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "did not finish") {
				t.Fatalf("list=%v warnings=%q", list, warnings)
			}
		}
	}
}

func TestPrivileges(t *testing.T) {
	e := newEnv(t, "")
	e.create(t, "a")

	other := e.manager(t, nil, e.owner.UID+1)
	if _, err := other.Create(context.Background(), "b"); !errors.Is(err, identity.ErrPrivilegeRequired) {
		t.Fatalf("Create as another user = %v", err)
	}
	if _, err := other.Rename(context.Background(), "a", "b"); !errors.Is(err, identity.ErrPrivilegeRequired) {
		t.Fatalf("Rename as another user = %v", err)
	}

	// As root, the manager drops to the owner once, after probing and
	// before its first mutation. (The owner here is the test user, so no
	// real ownership changes.)
	if e.owner.UID == 0 {
		// Already root: hand the storage to a synthetic owner so there is
		// a user to drop to. The fake DropTo leaves this process root.
		e.owner = identity.Identity{Username: "synthetic", UID: 4242, GID: 4242}
	}
	root := e.manager(t, nil, 0)
	f := &dropCheck{fsOps: osOps{}, e: e}
	root.ops = f
	if _, err := root.Create(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Rename(context.Background(), "c", "d"); err != nil {
		t.Fatal(err)
	}
	if len(e.dropped) != 1 || e.dropped[0] != e.owner || f.early {
		t.Fatalf("dropped=%v early=%v", e.dropped, f.early)
	}
}

type dropCheck struct {
	fsOps
	e     *env
	early bool
}

func (d *dropCheck) Mkdir(p string, m fs.FileMode) error {
	if len(d.e.dropped) == 0 {
		d.early = true
	}
	return d.fsOps.Mkdir(p, m)
}

func (d *dropCheck) Rename(a, b string) error {
	if len(d.e.dropped) == 0 {
		d.early = true
	}
	return d.fsOps.Rename(a, b)
}

func TestLockServerDetectsRename(t *testing.T) {
	e := newEnv(t, "")
	dir := e.create(t, "a")
	l, s, err := e.mgr.LockServer("a")
	if err != nil || s.Name != "a" {
		t.Fatal(err)
	}
	l.Release()

	// A lock taken on a directory that is then renamed no longer
	// verifies, which is what a waiter observes.
	held, err := filelock.Acquire(filelock.ServerLockPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	os.Rename(dir, filepath.Join(e.root, "b"))
	if err := verifyLock(held); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verifyLock after rename = %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	p := &fakeProber{}
	for _, cfg := range []Config{
		{Root: "relative", PropertiesFile: "server.properties", Prober: p},
		{Root: "/", PropertiesFile: "server.properties", Prober: p},
		{Root: "/opt/msm/servers", PropertiesFile: "../x", Prober: p},
		{Root: "/opt/msm/servers", PropertiesFile: "", Prober: p},
		{Root: "/opt/msm/servers", PropertiesFile: "server.properties"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
}
