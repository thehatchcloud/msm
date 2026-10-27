package worlds

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func isTmpfs(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) == nil && int64(st.Type) == tmpfsMagic
}

// preparedRAMVolume is a directory on /dev/shm, the tmpfs every systemd
// Linux mounts.
func preparedRAMVolume(t *testing.T) string {
	t.Helper()
	if !isTmpfs("/dev/shm") {
		requireRAM(t, "/dev/shm is not a tmpfs")
	}
	dir, err := os.MkdirTemp("/dev/shm", "msm-ramtest-")
	if err != nil {
		requireRAM(t, "/dev/shm is not writable: "+err.Error())
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// diskDirectory is a directory that is not tmpfs: the working directory
// (the source tree), else the temporary directory.
func diskDirectory(t *testing.T) string {
	t.Helper()
	for _, d := range []string{".", t.TempDir()} {
		if !isTmpfs(d) {
			abs, err := os.Getwd()
			if d != "." || err != nil {
				return d
			}
			return abs
		}
	}
	t.Skip("no directory on a disk filesystem here")
	return ""
}

func TestLinuxRefusesRamfsAndDisk(t *testing.T) {
	if isTmpfs(".") {
		t.Skip("the working directory is a tmpfs")
	}
	if err := checkRAMBacked(".", nil); err == nil {
		t.Errorf("a disk directory was accepted")
	}
}
