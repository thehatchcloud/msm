package worlds

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireRAM skips an integration case that cannot run here, unless
// MSM_REQUIRE_RAM=1 (set in CI) turns the missing capability into a failure.
func requireRAM(t *testing.T, why string) {
	t.Helper()
	if os.Getenv("MSM_REQUIRE_RAM") == "1" {
		t.Fatalf("a RAM volume is required: %s", why)
	}
	t.Skip(why)
}

// TestRealRAMVolume runs the whole life of a RAM world on a RAM volume the
// platform's own tools prepared, with the real volume check and no fake:
// copy to RAM, serve, synchronize, leave RAM.
func TestRealRAMVolume(t *testing.T) {
	ram := preparedRAMVolume(t)
	e := newEnv(t, "RAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH="+filepath.Join(ram, "msm")+"\n")
	e.wm.ramCheck = func(p string) error { return checkRAMBacked(p, execProgram) }
	dir := e.create(t, "s")
	active := filepath.Join(dir, "worldstorage")
	world(t, active, "w")
	ctx := context.Background()

	if err := e.wm.ToggleRAM(ctx, "s", "w", e.output()); err != nil {
		t.Fatalf("ToggleRAM on a RAM volume: %v", err)
	}
	copyDir := filepath.Join(ram, "msm", "s", "w")
	mustExist(t, filepath.Join(copyDir, "level.dat"))
	if err := e.wm.Load(ctx, "s", e.output()); err != nil {
		t.Fatal(err)
	}
	if got := readlink(t, filepath.Join(dir, "w")); got != copyDir {
		t.Errorf("link = %q, want %q", got, copyDir)
	}
	bump(t, filepath.Join(copyDir, "level.dat"), "played on a RAM volume")
	if err := e.wm.ToDisk(ctx, "s", e.output()); err != nil {
		t.Fatalf("ToDisk: %v", err)
	}
	if got := readFile(t, filepath.Join(active, "w", "level.dat")); got != "played on a RAM volume" {
		t.Errorf("disk copy = %q", got)
	}
	bump(t, filepath.Join(copyDir, "level.dat"), "last words")
	if err := e.wm.ToggleRAM(ctx, "s", "w", e.output()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(active, "w", "level.dat")); got != "last words" {
		t.Errorf("disk copy = %q", got)
	}
	mustNotExist(t, copyDir)
}

// A directory on the ordinary disk is refused as RAM, with the real check.
func TestDiskDirectoryIsNotRAM(t *testing.T) {
	disk := diskDirectory(t)
	if err := checkRAMBacked(disk, execProgram); err == nil {
		t.Fatalf("%s was accepted as RAM", disk)
	} else if !strings.Contains(err.Error(), "disk") {
		t.Errorf("error = %v", err)
	}
	e := newEnv(t, "RAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH="+filepath.Join(disk, "msm-ram")+"\n")
	e.wm.ramCheck = func(p string) error { return checkRAMBacked(p, execProgram) }
	dir := e.create(t, "s")
	world(t, filepath.Join(dir, "worldstorage"), "w")
	err := e.wm.ToggleRAM(context.Background(), "s", "w", e.output())
	if err == nil || !strings.Contains(err.Error(), "does not create or mount RAM disks") {
		t.Fatalf("ToggleRAM = %v", err)
	}
	mustNotExist(t, filepath.Join(disk, "msm-ram"))
	mustNotExist(t, filepath.Join(dir, "worldstorage", "w", "inram"))
}
