package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

// withRAM points the environment's servers at a RAM volume: a directory on
// /dev/shm, which the real volume check accepts as tmpfs. MSM_REQUIRE_RAM=1
// (CI) turns a machine without one into a failure.
func (e *env) withRAM(t *testing.T) string {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs("/dev/shm", &st); err != nil || int64(st.Type) != 0x01021994 {
		if os.Getenv("MSM_REQUIRE_RAM") == "1" {
			t.Fatal("/dev/shm is not a tmpfs")
		}
		t.Skip("/dev/shm is not a tmpfs")
	}
	ram, err := os.MkdirTemp("/dev/shm", "msm-lifecycle-")
	if err != nil {
		t.Skip("/dev/shm is not writable")
	}
	t.Cleanup(func() { os.RemoveAll(ram) })
	conf := filepath.Join(t.TempDir(), "msm.conf")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf("RAMDISK_STORAGE_ENABLED=true\nRAMDISK_STORAGE_PATH=%s\n", filepath.Join(ram, "msm"))), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := legacyconf.Load(conf)
	if err != nil {
		t.Fatal(err)
	}
	me := identity.Identity{Username: "tester", UID: os.Geteuid(), GID: os.Getegid()}
	euid := me.UID
	m, err := servers.New(servers.Config{Root: e.root, PropertiesFile: "server.properties", Global: g,
		Owner: me, DefaultOwner: &me, Prober: stoppedProber{}, EUID: &euid})
	if err != nil {
		t.Fatal(err)
	}
	e.servers = m
	return filepath.Join(ram, "msm")
}

// Start serves a flagged world from RAM; a stop writes it back to disk
// before it reports success, and a stop that cannot make the worlds durable
// says so.
func TestStopWritesRAMWorldsToDisk(t *testing.T) {
	e := newEnv(t)
	e.linkWorlds = true
	ram := e.withRAM(t)
	dir := e.server("a", behaveOK, "")
	disk := filepath.Join(dir, "worldstorage", "main")
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, data := range map[string]string{"level.dat": "level", "inram": ""} {
		if err := os.WriteFile(filepath.Join(disk, f), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := e.manager()
	var out strings.Builder
	if err := m.Start(bg, "a", &out); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	copyDir := filepath.Join(ram, "a", "main")
	if got, err := os.Readlink(filepath.Join(dir, "main")); err != nil || got != copyDir {
		t.Fatalf("link = %q, %v; want the RAM copy %s", got, err, copyDir)
	}
	// The running server plays in RAM.
	play := filepath.Join(copyDir, "level.dat")
	if err := os.WriteFile(play, []byte("played"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := m.Stop(bg, "a", true, &out); err != nil {
		t.Fatalf("stop: %v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(disk, "level.dat")); string(b) != "played" {
		t.Errorf("disk copy = %q after the stop", b)
	}
	if !strings.Contains(out.String(), `Synchronized world "main" from RAM to disk.`) {
		t.Errorf("stop output lacks the synchronization:\n%s", out.String())
	}

	// Without its manifest a RAM copy is not trusted, and the stop says the
	// worlds were not made durable; the copy is left alone.
	out.Reset()
	if err := m.Start(bg, "a", &out); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(copyDir, ".msm-ram.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(play, []byte("played again"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := m.Stop(bg, "a", true, &out)
	if err == nil || !strings.Contains(err.Error(), "its RAM worlds were not all written to disk") {
		t.Fatalf("stop = %v", err)
	}
	if e.screen.running("a") {
		t.Errorf("the server should be stopped even though its sync failed")
	}
	if b, _ := os.ReadFile(play); string(b) != "played again" {
		t.Errorf("RAM copy = %q, want it intact", b)
	}
}
