package worlds

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strings"
)

var sliceSuffix = regexp.MustCompile(`s\d+$`)

// ramDeviceListed reports whether device (a volume's /dev/diskNsM, or the
// whole disk) belongs to an image in the output of "hdiutil info" whose
// image-path is ram://.
func ramDeviceListed(info []byte, device string) bool {
	whole := sliceSuffix.ReplaceAllString(filepath.Base(device), "")
	for _, block := range bytes.Split(info, []byte("================================================")) {
		isRAM := false
		var devs []string
		for _, line := range strings.Split(string(block), "\n") {
			switch {
			case strings.HasPrefix(line, "image-path") && strings.Contains(line, "ram://"):
				isRAM = true
			case strings.HasPrefix(line, "/dev/disk"):
				devs = append(devs, sliceSuffix.ReplaceAllString(filepath.Base(strings.Fields(line)[0]), ""))
			}
		}
		if !isRAM {
			continue
		}
		for _, d := range devs {
			if d == whole {
				return true
			}
		}
	}
	return false
}
