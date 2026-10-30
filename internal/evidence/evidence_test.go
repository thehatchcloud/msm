package evidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func all(commit string) []File {
	var files []File
	for _, tg := range Targets {
		for _, major := range JavaMajors {
			f := File{Schema: 1, OS: tg.OS, Arch: tg.Arch, JavaMajor: major, Commit: commit, BinarySHA256: tg.OS + tg.Arch}
			for _, s := range Scenarios {
				f.Results = append(f.Results, Result{Name: s, Passed: true})
			}
			files = append(files, f)
			if tg.OS == "linux" {
				r := f
				r.Root = true
				r.Results = []Result{{Name: RootScenario, Passed: true}}
				files = append(files, r)
			}
		}
	}
	return files
}

func TestCompleteEvidenceHasNoGaps(t *testing.T) {
	if gaps := Check(all(commit), commit, nil); len(gaps) != 0 {
		t.Fatalf("gaps: %v", gaps)
	}
}

func TestMissingRunnerIsAnExplicitBlocker(t *testing.T) {
	var files []File
	for _, f := range all(commit) {
		if f.OS == "linux" && f.Arch == "arm64" {
			continue
		}
		files = append(files, f)
	}
	gaps := Check(files, commit, nil)
	if len(gaps) == 0 || !strings.Contains(gaps[0], "BLOCKED linux/arm64") || !strings.Contains(strings.Join(gaps, "\n"), "no runner ran the suite") {
		t.Fatalf("gaps = %v", gaps)
	}
}

func TestFailedWrongCommitAndWrongBinaryAreBlockers(t *testing.T) {
	files := all(commit)
	files[0].Results[2].Passed = false // a scenario failed
	files[2].Commit = "ffffffffffffffffffffffffffffffffffffffff"
	gaps := strings.Join(Check(files, commit, nil), "\n")
	if !strings.Contains(gaps, `scenario "attach-detach" did not pass`) || !strings.Contains(gaps, "the evidence is for commit ffffffffffff") {
		t.Errorf("gaps:\n%s", gaps)
	}
	gaps = strings.Join(Check(all(commit), commit, map[string]string{"linux/amd64": "x", "linux/arm64": "linuxarm64",
		"darwin/amd64": "darwinamd64", "darwin/arm64": "darwinarm64"}), "\n")
	if !strings.Contains(gaps, "linux/amd64") || !strings.Contains(gaps, "different executable") || strings.Contains(gaps, "darwin") {
		t.Errorf("gaps:\n%s", gaps)
	}
}

func TestMacOSDoesNotNeedTheRootScenario(t *testing.T) {
	for _, f := range all(commit) {
		if f.OS == "darwin" && f.Root {
			t.Fatal("test data has a macOS root file")
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "artifact-1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, f := range all(commit)[:3] {
		data, _ := json.Marshal(f)
		if err := os.WriteFile(filepath.Join(sub, "e2e-"+string(rune('a'+i))+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sub, "notes.json"), []byte("not evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := Load(dir)
	if err != nil || len(files) != 3 {
		t.Fatalf("Load = %d files, %v", len(files), err)
	}
}
