package buildinfo

import (
	"strings"
	"testing"
)

func TestCurrent(t *testing.T) {
	info := Current()
	if info.Version == "" || info.Commit == "" {
		t.Fatal("missing build metadata")
	}
	if !strings.Contains(info.String(), "(Go port)") {
		t.Fatal("version must distinguish the port from legacy MSM")
	}
}
