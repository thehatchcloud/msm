package lifecycle

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// findProgram resolves the invocation's program the way screen will run
// it: a name without '/' is searched on path, a relative path is taken from
// the server directory. It must be an executable regular file.
func findProgram(program, dir, path string) (string, error) {
	var candidates []string
	switch {
	case filepath.IsAbs(program):
		candidates = []string{program}
	case strings.Contains(program, "/"):
		candidates = []string{filepath.Join(dir, program)}
	default:
		for _, d := range filepath.SplitList(path) {
			if filepath.IsAbs(d) {
				candidates = append(candidates, filepath.Join(d, program))
			}
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	where := "on PATH " + path
	if len(candidates) == 1 && strings.Contains(program, "/") {
		where = "at " + candidates[0]
	}
	return "", fmt.Errorf("%w: %q was not found %s; install Java, or set msm-invocation in the server's properties", ErrNoJava, program, where)
}

// jarMagic starts every JAR, which is a ZIP archive.
var jarMagic = []byte("PK\x03\x04")

// checkJAR requires JAR_PATH to be a readable ZIP archive, as
// server_ensure_jar requires it to exist, so a missing or corrupt JAR is
// reported before anything is launched.
func checkJAR(name, jar string) error {
	f, err := os.OpenFile(jar, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: server %q has no JAR at %s; choose a server JAR first", ErrNoJAR, name, jar)
	}
	if err != nil {
		return fmt.Errorf("%w: server %q: %v", ErrNoJAR, name, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: server %q: %s is not a regular file", ErrInvalidJAR, name, jar)
	}
	head := make([]byte, len(jarMagic))
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head, jarMagic) {
		return fmt.Errorf("%w: server %q: %s is not a JAR (ZIP) file", ErrInvalidJAR, name, jar)
	}
	return nil
}

var eulaAccepted = regexp.MustCompile(`(?i)eula=true`)

// eulaRejected reports whether the server directory's eula.txt exists
// without accepting the EULA, the test server_start makes after a failed
// start.
func eulaRejected(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "eula.txt"))
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for scanner.Scan() {
		if eulaAccepted.MatchString(scanner.Text()) {
			return false
		}
	}
	return true
}
