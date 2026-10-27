package worlds

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// preparedRAMVolume attaches a 32 MiB RAM disk and formats and mounts it, the
// way an administrator prepares one on macOS.
func preparedRAMVolume(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("hdiutil", "attach", "-nomount", "ram://65536").Output()
	if err != nil {
		requireRAM(t, "cannot attach a RAM disk: "+err.Error())
	}
	dev := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("hdiutil", "detach", dev, "-force").Run() })
	name := "msmtest" + strings.ReplaceAll(strings.TrimPrefix(dev, "/dev/"), "disk", "")
	if out, err := exec.Command("diskutil", "erasevolume", "HFS+", name, dev).CombinedOutput(); err != nil {
		requireRAM(t, "cannot format the RAM disk: "+err.Error()+": "+string(out))
	}
	return "/Volumes/" + name
}

func diskDirectory(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
