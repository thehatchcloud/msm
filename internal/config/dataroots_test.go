package config

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/thehatchcloud/msm/internal/legacyconf"
)

func TestResolveDataRootsLegacyAlwaysWins(t *testing.T) {
	legacy := &legacyconf.GlobalSettings{
		ServerStoragePath: "/srv/minecraft/servers",
		JarStoragePath:    "/srv/minecraft/jars",
	}
	for _, euid := range []int{0, 1000} {
		got, err := ResolveDataRoots(legacy, euid)
		if err != nil {
			t.Fatal(err)
		}
		want := DataRoots{ServerStoragePath: "/srv/minecraft/servers", JarStoragePath: "/srv/minecraft/jars"}
		if got != want {
			t.Errorf("ResolveDataRoots(legacy, euid=%d) = %+v, want %+v", euid, got, want)
		}
	}
}

func TestResolveDataRootsRootWithoutLegacyKeepsClassicLayout(t *testing.T) {
	got, err := ResolveDataRoots(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := DataRoots{
		ServerStoragePath: legacyconf.DefaultServerStoragePath,
		JarStoragePath:    legacyconf.DefaultJarStoragePath,
	}
	if got != want {
		t.Fatalf("ResolveDataRoots(nil, root) = %+v, want the classic %+v", got, want)
	}
}

func TestResolveDataRootsUnprivilegedWithoutLegacyIsRootless(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/home/example/.local/share")

	got, err := ResolveDataRoots(nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := DataRoots{
		ServerStoragePath: "/home/example/.local/share/msm/servers",
		JarStoragePath:    "/home/example/.local/share/msm/jars",
	}
	if got != want {
		t.Fatalf("ResolveDataRoots(nil, unprivileged) = %+v, want %+v", got, want)
	}
}

func TestResolveDataRootsUnprivilegedNeverEqualsRootDefaults(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/home/example/.local/share")

	rootless, err := ResolveDataRoots(nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if rootless.ServerStoragePath == legacyconf.DefaultServerStoragePath {
		t.Fatal("an unprivileged fresh install must not silently reuse the root-owned /opt/msm layout")
	}
}

func TestRootlessDataDirUsesThePlatformConvention(t *testing.T) {
	t.Setenv("HOME", "/home/example")
	t.Setenv("XDG_DATA_HOME", "")

	got, err := rootlessDataDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/example", ".local", "share", "msm")
	if runtime.GOOS == "darwin" {
		want = filepath.Join("/home/example", "Library", "Application Support", "msm")
	}
	if got != want {
		t.Fatalf("rootlessDataDir() = %q, want %q", got, want)
	}
}
