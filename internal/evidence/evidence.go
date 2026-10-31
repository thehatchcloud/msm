// Package evidence defines the runtime validation evidence a native runner
// writes (internal/e2e) and the release check that reads it
// (msm-release evidence). Cross-compiling a target proves the code builds
// for it; this is the record that the executable also ran there, against a
// real GNU screen and a real JVM.
package evidence

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Result is one scenario's outcome.
type Result struct {
	Name    string  `json:"name"`
	Passed  bool    `json:"passed"`
	Seconds float64 `json:"seconds"`
}

// File is what one run of the end-to-end suite records.
type File struct {
	Schema       int      `json:"schema"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Go           string   `json:"go"`
	Java         string   `json:"java"`
	JavaMajor    int      `json:"java_major"`
	Screen       string   `json:"screen"`
	Commit       string   `json:"commit"`
	Version      string   `json:"version"`
	BinarySHA256 string   `json:"binary_sha256"`
	Time         string   `json:"time"`
	Root         bool     `json:"root"`
	Results      []Result `json:"results"`
}

// Targets are the platforms a release ships.
var Targets = []struct{ OS, Arch string }{
	{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"},
}

// JavaMajors are the Java versions the suite is declared to cover
// (docs/validation.md).
var JavaMajors = []int{17, 21}

// Scenarios the suite must have passed on a target, run as the ordinary
// user. The same names appear in internal/e2e.
var Scenarios = []string{
	"start", "commands", "attach-detach", "graceful-stop", "backup-and-restore",
	"global-intent", "crash-recovery", "ram-sync", "other-user-is-refused",
}

// RootScenario is run as root; only Linux validates it (docs/validation.md).
const RootScenario = "root-acts-as-the-owner"

// Load reads every *.json file below dir that is an evidence file.
func Load(dir string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") || !strings.HasPrefix(d.Name(), "e2e-") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var f File
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		files = append(files, f)
		return nil
	})
	return files, err
}

// Check compares the evidence with what a release at commit needs and lists
// every gap; no gap means the release has the evidence. binaries, when not
// nil, maps "os/arch" to the SHA-256 of the executable that is being
// released, and each file must have been made with exactly that executable.
// Without it, only the commit ties the evidence to the release.
func Check(files []File, commit string, binaries map[string]string) []string {
	var gaps []string
	for _, t := range Targets {
		platform := t.OS + "/" + t.Arch
		for _, major := range JavaMajors {
			for _, root := range []bool{false, true} {
				if root && t.OS != "linux" {
					continue
				}
				who := "as a user"
				if root {
					who = "as root"
				}
				label := fmt.Sprintf("%s, Java %d, %s", platform, major, who)
				f, why := find(files, t.OS, t.Arch, major, root, commit)
				if f == nil {
					gaps = append(gaps, fmt.Sprintf("BLOCKED %s: %s", label, why))
					continue
				}
				if want, ok := binaries[platform]; binaries != nil && (!ok || f.BinarySHA256 != want) {
					gaps = append(gaps, fmt.Sprintf("BLOCKED %s: the evidence was made with a different executable than the release's (%s, release %s)", label, short(f.BinarySHA256), short(want)))
				}
				need := Scenarios
				if root {
					need = []string{RootScenario}
				}
				for _, s := range need {
					if !passed(f, s) {
						gaps = append(gaps, fmt.Sprintf("BLOCKED %s: scenario %q did not pass", label, s))
					}
				}
			}
		}
	}
	sort.Strings(gaps)
	return gaps
}

func find(files []File, goos, arch string, major int, root bool, commit string) (*File, string) {
	var other string
	for i := range files {
		f := &files[i]
		if f.OS != goos || f.Arch != arch || f.JavaMajor != major || f.Root != root {
			continue
		}
		if f.Commit != commit {
			other = fmt.Sprintf("the evidence is for commit %s, not %s", short(f.Commit), short(commit))
			continue
		}
		return f, ""
	}
	if other != "" {
		return nil, other
	}
	return nil, "no evidence: no runner ran the suite for this target (an unavailable runner is a release blocker, or the target is declared unsupported)"
}

func passed(f *File, name string) bool {
	for _, r := range f.Results {
		if r.Name == name {
			return r.Passed
		}
	}
	return false
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "none"
	}
	return s
}
