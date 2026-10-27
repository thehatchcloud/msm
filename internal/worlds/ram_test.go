package worlds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

// ramEnv is an environment with RAM worlds enabled: a "RAM volume" that is
// an ordinary temporary directory, which the permissive ramCheck of newEnv
// accepts, and one server "s" with a world "w" in active storage.
type ramEnv struct {
	*env
	ram, dir, active string
}

func newRAMEnv(t *testing.T) *ramEnv {
	t.Helper()
	ram := filepath.Join(t.TempDir(), "ram")
	e := newEnv(t, fmt.Sprintf(ramConf, ram))
	dir := e.create(t, "s")
	active := filepath.Join(dir, "worldstorage")
	world(t, active, "w")
	return &ramEnv{env: e, ram: ram, dir: dir, active: active}
}

func (e *ramEnv) copyOf(w string) string { return filepath.Join(e.ram, "s", w) }

func (e *ramEnv) toggle(t *testing.T, w string) error {
	t.Helper()
	return e.wm.ToggleRAM(context.Background(), "s", w, e.output())
}

func (e *ramEnv) settings(t *testing.T) *legacyconf.ServerSettings {
	t.Helper()
	s, err := e.sm.Settings("s")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *ramEnv) sync(t *testing.T, live bool) error {
	t.Helper()
	return e.wm.SyncToDisk(context.Background(), e.settings(t), live, e.output())
}

func (e *ramEnv) load(t *testing.T) {
	t.Helper()
	if err := e.wm.Load(context.Background(), "s", e.output()); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func (e *env) seedRAM(t *testing.T, server, name string) {
	t.Helper()
	s, err := e.sm.Settings(server)
	if err != nil {
		t.Fatal(err)
	}
	l, err := e.wm.load(s)
	if err != nil {
		t.Fatal(err)
	}
	w, err := l.find(name)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.wm.prepareRAM(s, l)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.seed(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// bump writes data to a file with a modification time later than any earlier
// write, so a fingerprint sees the change even in the same second.
func bump(t *testing.T, path, data string) {
	t.Helper()
	write(t, path, data)
	later := time.Now().Add(time.Hour + time.Duration(len(data))*time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// New installations have RAM worlds off, and so does a configuration that
// does not enable them; flagged worlds then stay on disk and the RAM
// commands say why.
func TestRAMOffByDefault(t *testing.T) {
	for name, conf := range map[string]string{
		"no config": "", "disabled": "RAMDISK_STORAGE_ENABLED=false\n",
		"unset":    "USERNAME=x\n",
		"relative": "RAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH=ram\n",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, conf)
			dir := e.create(t, "s")
			world(t, filepath.Join(dir, "worldstorage"), "w")
			err := e.wm.ToggleRAM(context.Background(), "s", "w", e.output())
			if !errors.Is(err, ErrRAMConfig) {
				t.Fatalf("ToggleRAM = %v, want ErrRAMConfig", err)
			}
			mustNotExist(t, filepath.Join(dir, "worldstorage", "w", "inram"))
			if err := e.wm.ToDisk(context.Background(), "s", e.output()); err != nil || !strings.Contains(e.out.String(), "RAM worlds are off") {
				t.Errorf("ToDisk = %v, %q", err, e.out.String())
			}
		})
	}
}

// The configuration shipped with a new installation leaves RAM worlds off.
func TestShippedConfigDefaultsRAMOff(t *testing.T) {
	f, err := legacyconf.Load("../../msm.conf")
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := f.Lookup("RAMDISK_STORAGE_ENABLED"); !ok || v != "false" {
		t.Errorf("msm.conf RAMDISK_STORAGE_ENABLED = %q, %v; want \"false\"", v, ok)
	}
}

// Imported legacy intent is kept: a flagged world stays flagged and listed,
// and it only starts to be served from RAM once the setting and the volume
// check allow it.
func TestImportedIntentWaitsForValidation(t *testing.T) {
	e := newRAMEnv(t)
	write(t, filepath.Join(e.active, "w", "inram"), "")
	e.wm.ramCheck = func(string) error { return errors.New("it is a disk directory") }
	e.load(t)
	if got := readlink(t, filepath.Join(e.dir, "w")); got != filepath.Join(e.active, "w") {
		t.Errorf("link = %q, want the disk copy", got)
	}
	if !strings.Contains(e.errw.String(), "is not available") && !strings.Contains(e.errw.String(), "RAM-backed") {
		t.Errorf("stderr = %q", e.errw.String())
	}
	mustExist(t, filepath.Join(e.active, "w", "inram"))
	mustNotExist(t, e.ram)
	if !strings.Contains(e.listing(t, "s"), "RAM  w") {
		t.Errorf("the flag was lost from the listing")
	}
	e.wm.ramCheck = func(string) error { return nil }
	e.load(t)
	if got := readlink(t, filepath.Join(e.dir, "w")); got != e.copyOf("w") {
		t.Errorf("link = %q, want the RAM copy once the volume is valid", got)
	}
}

func TestToggleRAMOnAndOff(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	mustExist(t, filepath.Join(e.active, "w", "inram"))
	mustExist(t, filepath.Join(e.copyOf("w"), "level.dat"))
	mustExist(t, filepath.Join(e.copyOf("w"), ramManifestName))
	mustNotExist(t, filepath.Join(e.copyOf("w"), "inram"))
	mustNotExist(t, filepath.Join(e.active, "w", ramManifestName))
	if !strings.Contains(e.out.String(), "Changes will only take effect after server is restarted.") {
		t.Errorf("stdout = %q", e.out.String())
	}

	bump(t, filepath.Join(e.copyOf("w"), "region", "r.0.0.mca"), "played")
	bump(t, filepath.Join(e.copyOf("w"), "new.dat"), "new")
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(e.active, "w", "region", "r.0.0.mca")); got != "played" {
		t.Errorf("disk copy = %q; the RAM changes were not written before RAM was left", got)
	}
	mustExist(t, filepath.Join(e.active, "w", "new.dat"))
	mustNotExist(t, filepath.Join(e.active, "w", "inram"))
	mustNotExist(t, filepath.Join(e.active, "w", ramManifestName))
	mustNotExist(t, e.copyOf("w"))
}

func TestToggleRAMRefusals(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.wm.ToggleRAM(context.Background(), "s", "ghost", e.output()); !errors.Is(err, ErrNoWorld) {
		t.Errorf("unknown world: %v", err)
	}
	world(t, filepath.Join(e.dir, "worldstorage_inactive"), "off")
	if err := e.toggle(t, "off"); err == nil || !strings.Contains(err.Error(), "not active") {
		t.Errorf("inactive world: %v", err)
	}
	e.prober.states["s"] = servers.State{Kind: servers.Running}
	if err := e.toggle(t, "w"); !errors.Is(err, servers.ErrRunning) {
		t.Errorf("running server: %v", err)
	}
	mustNotExist(t, filepath.Join(e.active, "w", "inram"))
	e.prober.states["s"] = servers.State{}
	// A copy this manager did not make is not touched.
	foreign := world(t, filepath.Join(e.ram, "s"), "w")
	if err := e.toggle(t, "w"); !errors.Is(err, ErrRAMUnmanaged) {
		t.Errorf("foreign copy: %v", err)
	}
	mustExist(t, filepath.Join(foreign, "level.dat"))
	mustNotExist(t, filepath.Join(e.active, "w", "inram"))
}

// A path that is not on a RAM-backed volume is refused with an error that
// says how to prepare one, and nothing is created.
func TestUnsupportedRAMPath(t *testing.T) {
	e := newRAMEnv(t)
	e.wm.ramCheck = func(p string) error {
		return fmt.Errorf("%s is not on a RAM-backed filesystem (tmpfs): it is a disk directory", p)
	}
	err := e.toggle(t, "w")
	if !errors.Is(err, ErrRAMUnsupported) {
		t.Fatalf("ToggleRAM = %v, want ErrRAMUnsupported", err)
	}
	for _, want := range []string{"disk directory", "does not create or mount RAM disks", "docs/ram-worlds.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	mustNotExist(t, e.ram)
	mustNotExist(t, filepath.Join(e.active, "w", "inram"))
}

// A mount that is not there is a configuration error, not a directory made
// on the root filesystem: the nearest existing part of the path is checked.
func TestMissingRAMMountIsRefused(t *testing.T) {
	e := newRAMEnv(t)
	var checked []string
	e.wm.ramCheck = func(p string) error {
		checked = append(checked, p)
		if p == filepath.Dir(e.ram) {
			return errors.New("not mounted")
		}
		return nil
	}
	if err := os.RemoveAll(e.ram); err != nil {
		t.Fatal(err)
	}
	if err := e.toggle(t, "w"); !errors.Is(err, ErrRAMUnsupported) {
		t.Fatalf("ToggleRAM = %v", err)
	}
	if len(checked) == 0 || checked[0] != filepath.Dir(e.ram) {
		t.Errorf("checked %v, want the existing parent first", checked)
	}
	mustNotExist(t, e.ram)

}

// After a reboot or an unmount the RAM copies are gone. A stopped server
// carries on from the disk copy of the last synchronization; a running one
// is in trouble and says so.
func TestRAMCopyLostIsReported(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(e.ram); err != nil {
		t.Fatal(err)
	}
	if err := e.sync(t, false); err != nil {
		t.Errorf("stopped server: %v", err)
	}
	if !strings.Contains(e.errw.String(), "has no RAM copy") {
		t.Errorf("stderr = %q", e.errw.String())
	}
	if err := e.sync(t, true); err == nil || !strings.Contains(err.Error(), "missing while the server runs") {
		t.Errorf("running server: %v", err)
	}
	// The disk copy is untouched.
	if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "level w" {
		t.Errorf("disk level.dat = %q", got)
	}
	// With the volume gone, the next start serves the disk copy until RAM
	// is available again, then makes a fresh RAM copy.
	e.wm.ramCheck = func(string) error { return errors.New("not mounted") }
	e.load(t)
	if got := readlink(t, filepath.Join(e.dir, "w")); got != filepath.Join(e.active, "w") {
		t.Errorf("link = %q", got)
	}
	e.wm.ramCheck = func(string) error { return nil }
	e.load(t)
	if got := readlink(t, filepath.Join(e.dir, "w")); got != e.copyOf("w") {
		t.Errorf("link = %q, want a fresh RAM copy", got)
	}
}

func TestENOSPCMakingRAMCopy(t *testing.T) {
	t.Run("free space check", func(t *testing.T) {
		e := newRAMEnv(t)
		e.wm.free = func(string) (uint64, error) { return 100, nil }
		err := e.toggle(t, "w")
		if !errors.Is(err, ErrRAMSpace) || !strings.Contains(err.Error(), "bytes needed") {
			t.Fatalf("ToggleRAM = %v, want ErrRAMSpace", err)
		}
		mustNotExist(t, filepath.Join(e.active, "w", "inram"))
		if got := entries(t, filepath.Join(e.ram, "s")); len(got) != 0 {
			t.Errorf("RAM holds %v", got)
		}
	})
	t.Run("full while copying", func(t *testing.T) {
		e := newRAMEnv(t)
		e.ops.fail = func(op string, _ ...string) error {
			if op == "CopyWorld" {
				return &os.PathError{Op: "write", Path: "x", Err: unix.ENOSPC}
			}
			return nil
		}
		err := e.toggle(t, "w")
		if !errors.Is(err, ErrRAMSpace) || !strings.Contains(err.Error(), "the disk copy is unchanged") {
			t.Fatalf("ToggleRAM = %v", err)
		}
		mustNotExist(t, filepath.Join(e.active, "w", "inram"))
		if got := entries(t, filepath.Join(e.ram, "s")); len(got) != 0 {
			t.Errorf("a partial copy was left in RAM: %v", got)
		}
		if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "level w" {
			t.Errorf("disk copy changed: %q", got)
		}
	})
}

// A full disk while writing a RAM world back leaves the disk copy as it
// was, removes the partial copy, keeps the RAM copy, and the next attempt
// works.
func TestENOSPCWritingToDisk(t *testing.T) {
	for name, setup := range map[string]func(*ramEnv){
		"free space check": func(e *ramEnv) {
			e.wm.free = func(p string) (uint64, error) {
				if p == e.active {
					return 10, nil
				}
				return 1 << 40, nil
			}
		},
		"full while copying": func(e *ramEnv) {
			e.ops.fail = func(op string, args ...string) error {
				if op == "CopyWorld" && strings.Contains(args[1], syncStage) {
					return &os.PathError{Op: "write", Path: "x", Err: unix.ENOSPC}
				}
				return nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRAMEnv(t)
			if err := e.toggle(t, "w"); err != nil {
				t.Fatal(err)
			}
			bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "newer")
			setup(e)
			err := e.sync(t, false)
			if !errors.Is(err, ErrRAMSpace) && (err == nil || !strings.Contains(err.Error(), "could not be written to disk")) {
				t.Fatalf("SyncToDisk = %v", err)
			}
			if !strings.Contains(e.errw.String(), "keeps its last synchronized state") {
				t.Errorf("stderr = %q", e.errw.String())
			}
			if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "level w" {
				t.Errorf("disk copy = %q, want the last synchronized state", got)
			}
			if got := readFile(t, filepath.Join(e.copyOf("w"), "level.dat")); got != "newer" {
				t.Errorf("RAM copy = %q, want it intact", got)
			}
			for _, n := range entries(t, e.active) {
				if strings.HasPrefix(n, stagePrefix) {
					t.Errorf("leftover %s", n)
				}
			}
			e.ops.fail, e.wm.free = nil, freeBytes
			if err := e.sync(t, false); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "newer" {
				t.Errorf("disk copy = %q after the retry", got)
			}
		})
	}
}

func TestInterruptedCopies(t *testing.T) {
	t.Run("to RAM", func(t *testing.T) {
		e := newRAMEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		e.ops.fail = func(op string, _ ...string) error {
			if op == "CopyWorld" {
				cancel()
			}
			return nil
		}
		err := e.wm.ToggleRAM(ctx, "s", "w", e.output())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ToggleRAM = %v", err)
		}
		mustNotExist(t, filepath.Join(e.active, "w", "inram"))
		if got := entries(t, filepath.Join(e.ram, "s")); len(got) != 0 {
			t.Errorf("RAM holds %v", got)
		}
	})
	t.Run("to disk", func(t *testing.T) {
		e := newRAMEnv(t)
		if err := e.toggle(t, "w"); err != nil {
			t.Fatal(err)
		}
		bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "newer")
		ctx, cancel := context.WithCancel(context.Background())
		e.ops.fail = func(op string, _ ...string) error {
			if op == "CopyWorld" {
				cancel()
			}
			return nil
		}
		if err := e.wm.SyncToDisk(ctx, e.settings(t), false, e.output()); !errors.Is(err, context.Canceled) && (err == nil || !strings.Contains(err.Error(), "context canceled")) {
			t.Fatalf("SyncToDisk = %v", err)
		}
		if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "level w" {
			t.Errorf("disk copy = %q", got)
		}
		for _, n := range entries(t, e.active) {
			if strings.HasPrefix(n, stagePrefix) {
				t.Errorf("leftover %s", n)
			}
		}
	})
	t.Run("killed leftovers are swept", func(t *testing.T) {
		e := newRAMEnv(t)
		if err := e.toggle(t, "w"); err != nil {
			t.Fatal(err)
		}
		bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "newer")
		write(t, filepath.Join(e.active, syncStage+"w-dead", "junk"), "x")
		write(t, filepath.Join(e.ram, "s", ramStagePrefix+"w-dead", "junk"), "x")
		if err := e.sync(t, false); err != nil {
			t.Fatal(err)
		}
		mustNotExist(t, filepath.Join(e.active, syncStage+"w-dead"))
		mustNotExist(t, filepath.Join(e.ram, "s", ramStagePrefix+"w-dead"))
		if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "newer" {
			t.Errorf("disk copy = %q", got)
		}
	})
}

// An exchange-less filesystem gets two renames; if the second fails, the
// previous copy is put back.
func TestPublishWithoutExchange(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	e.ops.noExchange = true
	bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "newer")
	e.ops.fail = func(op string, args ...string) error {
		if op == "Rename" && strings.Contains(args[0], syncStage) {
			return unix.EIO
		}
		return nil
	}
	if err := e.sync(t, false); err == nil {
		t.Fatal("a failed rename was reported as done")
	}
	if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "level w" {
		t.Errorf("disk copy = %q; the previous copy was not put back", got)
	}
	e.ops.fail = nil
	if err := e.sync(t, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "newer" {
		t.Errorf("disk copy = %q", got)
	}
	for _, n := range entries(t, e.active) {
		if strings.HasPrefix(n, stagePrefix) {
			t.Errorf("leftover %s", n)
		}
	}
}

// Stale RAM data: a disk copy that changed after the RAM copy was made is
// the newer one. It replaces the RAM copy at the next start and is never
// overwritten by a todisk of the older RAM data.
func TestStaleRAMDataNeverOverwritesDisk(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	// Restored from a backup, say, while the server was off.
	bump(t, filepath.Join(e.active, "w", "level.dat"), "restored")
	if err := e.sync(t, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "restored" {
		t.Fatalf("todisk overwrote the newer disk copy with %q", got)
	}
	if got := readFile(t, filepath.Join(e.copyOf("w"), "level.dat")); got != "level w" {
		t.Errorf("RAM copy = %q, want it untouched by todisk", got)
	}
	e.load(t)
	if got := readFile(t, filepath.Join(e.copyOf("w"), "level.dat")); got != "restored" {
		t.Errorf("RAM copy = %q after start, want the restored world", got)
	}
	if !strings.Contains(e.out.String(), "changed on disk since it was copied to RAM") {
		t.Errorf("stdout = %q", e.out.String())
	}
	if got := readlink(t, filepath.Join(e.dir, "w")); got != e.copyOf("w") {
		t.Errorf("link = %q", got)
	}
}

// RAM data that is newer than disk (the manager or machine died before the
// last sync, RAM survived) is kept at the next start and written at the
// next sync, not overwritten by the disk copy.
func TestAheadRAMKeptAtStart(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "unsynced")
	e.load(t)
	if got := readFile(t, filepath.Join(e.copyOf("w"), "level.dat")); got != "unsynced" {
		t.Errorf("RAM copy = %q, want its unsynced data kept", got)
	}
	if !strings.Contains(e.out.String(), "holds changes not yet written to disk") {
		t.Errorf("stdout = %q", e.out.String())
	}
	if err := e.sync(t, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "unsynced" {
		t.Errorf("disk copy = %q", got)
	}
}

// Both sides changed: neither is overwritten, the server serves the disk
// copy, and leaving RAM is refused.
func TestBothSidesChanged(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "ram side")
	bump(t, filepath.Join(e.active, "w", "level.dat"), "disk side")

	e.load(t)
	if got := readlink(t, filepath.Join(e.dir, "w")); got != filepath.Join(e.active, "w") {
		t.Errorf("link = %q, want the disk copy", got)
	}
	if !strings.Contains(e.errw.String(), "both in") {
		t.Errorf("stderr = %q", e.errw.String())
	}
	if err := e.sync(t, false); err == nil || !strings.Contains(e.errw.String(), ErrRAMConflict.Error()) {
		t.Errorf("SyncToDisk = %v, stderr %q", err, e.errw.String())
	}
	if err := e.toggle(t, "w"); !errors.Is(err, ErrRAMConflict) {
		t.Errorf("leaving RAM = %v, want ErrRAMConflict", err)
	}
	mustExist(t, filepath.Join(e.active, "w", "inram"))
	if got := readFile(t, filepath.Join(e.copyOf("w"), "level.dat")); got != "ram side" {
		t.Errorf("RAM copy = %q", got)
	}
	if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "disk side" {
		t.Errorf("disk copy = %q", got)
	}
}

// Writing to disk keeps the flag, changes only when RAM changed, and while
// the server runs only a world actually served from RAM is written.
func TestSyncToDiskDetails(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	e.load(t)
	before, err := os.Stat(filepath.Join(e.active, "w"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.sync(t, true); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(filepath.Join(e.active, "w")); !os.SameFile(before, after) {
		t.Errorf("an unchanged world was rewritten")
	}
	bump(t, filepath.Join(e.copyOf("w"), "region", "r.0.0.mca"), "played")
	if err := e.sync(t, true); err != nil {
		t.Fatal(err)
	}
	mustExist(t, filepath.Join(e.active, "w", "inram"))
	if got := readFile(t, filepath.Join(e.active, "w", "region", "r.0.0.mca")); got != "played" {
		t.Errorf("disk copy = %q", got)
	}
	if !strings.Contains(e.listing(t, "s"), "RAM  w") {
		t.Errorf("the world lost its RAM flag")
	}
	// Linked to disk (as after a conflict): a running server's disk world
	// is never swapped under it.
	bump(t, filepath.Join(e.copyOf("w"), "region", "r.0.0.mca"), "played more")
	if err := os.Remove(filepath.Join(e.dir, "w")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.active, "w"), filepath.Join(e.dir, "w")); err != nil {
		t.Fatal(err)
	}
	live, _ := os.Stat(filepath.Join(e.active, "w"))
	if err := e.sync(t, true); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.Stat(filepath.Join(e.active, "w")); !os.SameFile(live, now) {
		t.Errorf("the disk world a running server uses was replaced")
	}
	if !strings.Contains(e.errw.String(), "not being served from RAM") {
		t.Errorf("stderr = %q", e.errw.String())
	}
}

// removeCopy only ever removes a world's own directory inside the server's
// RAM directory.
func TestRemoveCopyIsContained(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "precious")
	write(t, filepath.Join(victim, "data"), "keep")
	// A world name cannot escape (validWorldName), and a copy that is a
	// link is not followed.
	if err := os.RemoveAll(e.copyOf("w")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, e.copyOf("w")); err != nil {
		t.Fatal(err)
	}
	s := e.settings(t)
	l, _ := e.wm.load(s)
	r, err := e.wm.prepareRAM(s, l)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.removeCopy(World{Name: "w"}); err == nil {
		t.Errorf("a linked copy was removed")
	}
	if err := r.removeCopy(World{Name: "../../escape"}); err == nil {
		t.Errorf("a path outside the RAM directory was accepted")
	}
	mustExist(t, filepath.Join(victim, "data"))
}

// Shutdown, backup and a cron todisk can all want to write at once. Each
// holds the server's lock, so a reader that holds it too (as a backup does)
// never sees a half-written world, and nothing is left behind.
func TestSimultaneousShutdownBackupAndPeriodicSync(t *testing.T) {
	e := newRAMEnv(t)
	if err := e.toggle(t, "w"); err != nil {
		t.Fatal(err)
	}
	e.load(t)
	files := []string{"level.dat", "region/r.0.0.mca", "DIM-1/region/r.0.0.mca", "DIM1/region/r.0.0.mca"}
	gen := func(n int) {
		for _, f := range files {
			bump(t, filepath.Join(e.copyOf("w"), f), fmt.Sprintf("gen %03d %s", n, strings.Repeat("x", 4096)))
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var problems []string
	note := func(format string, a ...any) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	locked := func(fn func(s *legacyconf.ServerSettings)) {
		lock, s, err := e.sm.LockServer("s")
		if err != nil {
			note("lock: %v", err)
			return
		}
		defer lock.Release()
		fn(s)
	}
	// The server writes new generations of its world in RAM; the sync
	// roles each take the lock to write them.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(role int) {
			defer wg.Done()
			for n := 0; n < 4; n++ {
				locked(func(s *legacyconf.ServerSettings) {
					if err := e.wm.SyncToDisk(context.Background(), s, role == 1, Output{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}); err != nil {
						note("sync role %d: %v", role, err)
					}
				})
			}
		}(i)
	}
	wg.Add(1)
	go func() { // a backup
		defer wg.Done()
		for n := 0; n < 8; n++ {
			locked(func(*legacyconf.ServerSettings) {
				var first string
				for _, f := range files {
					b, err := os.ReadFile(filepath.Join(e.active, "w", f))
					if err != nil {
						note("backup read %s: %v", f, err)
						return
					}
					got := string(b)
					if f == "level.dat" {
						first = got
						continue
					}
					if strings.HasPrefix(first, "gen ") && !strings.HasPrefix(got, first[:7]) {
						note("backup saw %q beside %q", got[:7], first[:7])
					}
				}
			})
		}
	}()
	wg.Add(1)
	go func() { // the running server
		defer wg.Done()
		for n := 1; n <= 6; n++ {
			locked(func(*legacyconf.ServerSettings) { gen(n) })
		}
	}()
	wg.Wait()
	for _, p := range problems {
		t.Error(p)
	}
	if err := e.sync(t, false); err != nil {
		t.Fatal(err)
	}
	for _, f := range files[:1] {
		if a, b := readFile(t, filepath.Join(e.active, "w", f)), readFile(t, filepath.Join(e.copyOf("w"), f)); a != b {
			t.Errorf("%s: disk %q, RAM %q", f, a[:7], b[:7])
		}
	}
	for _, n := range entries(t, e.active) {
		if strings.HasPrefix(n, stagePrefix) {
			t.Errorf("leftover %s", n)
		}
	}
}

// fakeSaver records the order of calls to a "running server".
type fakeSaver struct {
	running bool
	calls   []string
	offErr  error
	allErr  error
	onErr   error
	during  func()
}

func (f *fakeSaver) Running(context.Context, *legacyconf.ServerSettings, Output) (bool, error) {
	return f.running, nil
}

func (f *fakeSaver) SaveOff(context.Context, *legacyconf.ServerSettings, Output) error {
	f.calls = append(f.calls, "save-off")
	return f.offErr
}

func (f *fakeSaver) SaveAll(context.Context, *legacyconf.ServerSettings, Output) error {
	f.calls = append(f.calls, "save-all")
	if f.during != nil {
		f.during()
	}
	return f.allErr
}

func (f *fakeSaver) SaveOn(context.Context, *legacyconf.ServerSettings, Output) error {
	f.calls = append(f.calls, "save-on")
	return f.onErr
}

func TestToDiskAroundARunningServer(t *testing.T) {
	setup := func(t *testing.T) (*ramEnv, *fakeSaver, *Manager) {
		e := newRAMEnv(t)
		if err := e.toggle(t, "w"); err != nil {
			t.Fatal(err)
		}
		e.load(t)
		f := &fakeSaver{running: true}
		f.during = func() { bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "saved") }
		return e, f, e.wm.WithSaver(f)
	}
	t.Run("order", func(t *testing.T) {
		e, f, m := setup(t)
		if err := m.ToDisk(context.Background(), "s", e.output()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(f.calls, ","); got != "save-off,save-all,save-on" {
			t.Errorf("calls = %s", got)
		}
		if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "saved" {
			t.Errorf("disk copy = %q", got)
		}
	})
	t.Run("saving is turned back on after a failure", func(t *testing.T) {
		e, f, m := setup(t)
		e.ops.fail = func(op string, args ...string) error {
			if op == "CopyWorld" && strings.Contains(args[1], syncStage) {
				return unix.ENOSPC
			}
			return nil
		}
		if err := m.ToDisk(context.Background(), "s", e.output()); err == nil {
			t.Fatal("a failed write was reported as done")
		}
		if got := strings.Join(f.calls, ","); got != "save-off,save-all,save-on" {
			t.Errorf("calls = %s", got)
		}
	})
	t.Run("saving is turned back on after an interruption", func(t *testing.T) {
		e, f, m := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		old := f.during
		f.during = func() { old(); cancel() }
		if err := m.ToDisk(ctx, "s", e.output()); err == nil {
			t.Fatal("an interrupted write was reported as done")
		}
		if got := strings.Join(f.calls, ","); got != "save-off,save-all,save-on" {
			t.Errorf("calls = %s", got)
		}
	})
	t.Run("refused save-off is left alone", func(t *testing.T) {
		e, f, m := setup(t)
		f.offErr = fmt.Errorf("%w: already off", ErrSaveRefused)
		if err := m.ToDisk(context.Background(), "s", e.output()); err == nil {
			t.Fatal("expected an error")
		}
		if got := strings.Join(f.calls, ","); got != "save-off" {
			t.Errorf("calls = %s", got)
		}
	})
	t.Run("save-on failure is reported", func(t *testing.T) {
		e, f, m := setup(t)
		f.onErr = errors.New("no answer")
		err := m.ToDisk(context.Background(), "s", e.output())
		if err == nil || !strings.Contains(err.Error(), "saving could not be turned back on") {
			t.Errorf("ToDisk = %v", err)
		}
	})
	t.Run("nothing in RAM touches no console", func(t *testing.T) {
		e := newRAMEnv(t)
		f := &fakeSaver{running: true}
		if err := e.wm.WithSaver(f).ToDisk(context.Background(), "s", e.output()); err != nil || len(f.calls) != 0 {
			t.Errorf("ToDisk = %v, calls %v", err, f.calls)
		}
	})
	t.Run("stopped server", func(t *testing.T) {
		e := newRAMEnv(t)
		if err := e.toggle(t, "w"); err != nil {
			t.Fatal(err)
		}
		bump(t, filepath.Join(e.copyOf("w"), "level.dat"), "later")
		f := &fakeSaver{}
		if err := e.wm.WithSaver(f).ToDisk(context.Background(), "s", e.output()); err != nil || len(f.calls) != 0 {
			t.Errorf("ToDisk = %v, calls %v", err, f.calls)
		}
		if got := readFile(t, filepath.Join(e.active, "w", "level.dat")); got != "later" {
			t.Errorf("disk copy = %q", got)
		}
		e.prober.states["s"] = servers.State{Kind: servers.Unknown}
		if err := e.wm.WithSaver(f).ToDisk(context.Background(), "s", e.output()); err == nil {
			t.Errorf("a server that cannot be proven stopped was synchronized")
		}
	})
}

func TestCopyWorld(t *testing.T) {
	src := world(t, t.TempDir(), "w")
	write(t, filepath.Join(src, "inram"), "")
	if err := os.Symlink("level.dat", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "level.dat"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1_600_000_000, 123_000_000)
	if err := os.Chtimes(filepath.Join(src, "level.dat"), old, old); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if err := copyWorld(context.Background(), src, dst, copyOptions{Skip: map[string]bool{"inram": true}}); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(dst, "inram"))
	if got := readlink(t, filepath.Join(dst, "dangling")); got != "/nonexistent/target" {
		t.Errorf("link = %q", got)
	}
	info, err := os.Stat(filepath.Join(dst, "level.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 || !info.ModTime().Equal(old) {
		t.Errorf("level.dat mode %v time %v, want 0640 and %v", info.Mode().Perm(), info.ModTime(), old)
	}
	a, err := scanTree(src, map[string]bool{"inram": true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := scanTree(dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != b.Fingerprint || a.Bytes != b.Bytes {
		t.Errorf("a copy does not fingerprint like its original")
	}
	bump(t, filepath.Join(dst, "level.dat"), "different")
	if c, _ := scanTree(dst, nil); c.Fingerprint == a.Fingerprint {
		t.Errorf("a changed file did not change the fingerprint")
	}
	// A new file in a directory does.
	write(t, filepath.Join(dst, "region", "extra"), "x")
	if d, _ := scanTree(dst, nil); d.Fingerprint == a.Fingerprint {
		t.Errorf("an added file did not change the fingerprint")
	}

	// Nothing but files, directories and links.
	if err := unix.Mkfifo(filepath.Join(src, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyWorld(context.Background(), src, filepath.Join(t.TempDir(), "x"), copyOptions{}); err == nil || !strings.Contains(err.Error(), "named pipe") {
		t.Errorf("a named pipe: %v", err)
	}
	// The destination is never replaced, and a source that is a link is not
	// followed.
	if err := copyWorld(context.Background(), src, dst, copyOptions{}); err == nil {
		t.Errorf("an existing destination was written into")
	}
	lnk := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(src, lnk); err != nil {
		t.Fatal(err)
	}
	if err := copyWorld(context.Background(), lnk, filepath.Join(t.TempDir(), "y"), copyOptions{}); err == nil {
		t.Errorf("a linked source was followed")
	}
}

// As root, copies are handed to the server's owner.
func TestCopyWorldHandsFilesToOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	src := world(t, t.TempDir(), "w")
	if err := os.Symlink("level.dat", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	owner := identity.Identity{Username: "nobody", UID: 4242, GID: 4343}
	if err := copyWorld(context.Background(), src, dst, copyOptions{Owner: &owner}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dst, filepath.Join(dst, "region"), filepath.Join(dst, "level.dat"), filepath.Join(dst, "link")} {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		if st.Uid != 4242 || st.Gid != 4343 {
			t.Errorf("%s belongs to %d:%d", p, st.Uid, st.Gid)
		}
	}
}

func TestRAMDeviceListed(t *testing.T) {
	info := []byte(`framework       : 600.40.1
driver          : 600.40.1
================================================
image-path      : /Users/runner/disk.dmg
image-alias     : /Users/runner/disk.dmg
shadow-path     : <none>
icon-path       : /System/Library/PrivateFrameworks/DiskImages.framework/Resources/CDiskImage.icns
image-type      : read/write disk image
system-entity-class : disk image
/dev/disk3          	GUID_partition_scheme          	
/dev/disk3s1        	Apple_HFS                      	/Volumes/Disk
================================================
image-path      : ram://2048
image-alias     : ram://2048
image-type      : read/write disk image
/dev/disk4          	Apple_partition_scheme         	
/dev/disk4s1        	Apple_HFS                      	/Volumes/RAM
`)
	for dev, want := range map[string]bool{"/dev/disk4s1": true, "/dev/disk4": true, "/dev/disk3s1": false, "/dev/disk1s1": false} {
		if got := ramDeviceListed(info, dev); got != want {
			t.Errorf("ramDeviceListed(%s) = %v, want %v", dev, got, want)
		}
	}
}
