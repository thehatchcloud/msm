// Package logwatch reads the lines a server writes to its console log after
// a mark was taken, so output that predates a command (or a start) can never
// be mistaken for its result (DEV-010).
package logwatch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"syscall"
)

const (
	// maxLogRead bounds how much of the log one poll reads.
	maxLogRead = 1 << 20
	// maxLogLine bounds one line; the rest of a longer line is skipped.
	maxLogLine = 64 << 10
	maxHints   = 5
)

// Log lines that explain why a server stopped during startup.
var (
	EULAHint     = regexp.MustCompile(`(?i)you need to agree to the eula`)
	PortHint     = regexp.MustCompile(`(?i)failed to bind to port|address already in use`)
	failureHints = regexp.MustCompile(EULAHint.String() + "|" + PortHint.String())
)

// Watcher reads only the lines a server writes to its console log after
// the watcher was made, so an old start or save line from an earlier run
// can never confirm a new one (DEV-010). A log that is replaced (Minecraft
// 1.7+ moves latest.log aside at startup) or truncated is read from its
// beginning; one that is appended to is read from where it ended.
type Watcher struct {
	path     string
	base     os.FileInfo // the log when the watcher was made; nil if none
	baseSize int64
	baseTail []byte // the last bytes before baseSize

	f       *os.File
	info    os.FileInfo
	off     int64
	partial []byte
	skip    bool // discarding the rest of an over-long line
	Hints   []string
}

// Watch marks the log's current end; Poll and Lines see only what follows.
func Watch(path string) (*Watcher, error) {
	w := &Watcher{path: path}
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("inspect log %s: %w", path, err)
	default:
		w.base, w.baseSize = info, info.Size()
		w.baseTail, err = readTail(path, info)
		if err != nil {
			return nil, err
		}
	}
	return w, nil
}

// tailBytes is how much of the log's end identifies it: a file system may
// give a replacement log the old one's inode number.
const tailBytes = 512

func readTail(path string, info os.FileInfo) ([]byte, error) {
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", path, err)
	}
	defer f.Close()
	n := min(info.Size(), tailBytes)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, info.Size()-n); err != nil {
		return nil, fmt.Errorf("read log %s: %w", path, err)
	}
	return buf, nil
}

// continues reports whether f is the log the watcher started from, still
// holding what it held then, so only what follows baseSize is new. Any
// other file, or the same inode rewritten, is read from its beginning.
func (w *Watcher) continues(f *os.File, info os.FileInfo) bool {
	if w.base == nil || !os.SameFile(w.base, info) || info.Size() < w.baseSize {
		return false
	}
	if info.Size() == w.baseSize && !info.ModTime().Equal(w.base.ModTime()) {
		return false
	}
	tail := make([]byte, len(w.baseTail))
	if _, err := f.ReadAt(tail, w.baseSize-int64(len(tail))); err != nil {
		return false
	}
	return bytes.Equal(tail, w.baseTail)
}

func (w *Watcher) Close() {
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
}

// poll reads the log's new lines and reports whether one matches re (nil
// matches nothing; the lines are still read for Hints).
func (w *Watcher) Poll(re *regexp.Regexp) (bool, error) {
	lines, err := w.Lines()
	if err != nil {
		return false, err
	}
	if re != nil {
		for _, l := range lines {
			if re.MatchString(l) {
				return true, nil
			}
		}
	}
	return false, nil
}

// Lines reads the complete lines written since the last call (or since
// Watch), without their line terminators.
func (w *Watcher) Lines() ([]string, error) {
	cur, err := os.Stat(w.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect log %s: %w", w.path, err)
	}
	if !cur.Mode().IsRegular() {
		return nil, fmt.Errorf("log %s is not a regular file", w.path)
	}
	if w.f == nil || !os.SameFile(w.info, cur) {
		if err := w.open(cur); err != nil || w.f == nil {
			return nil, err
		}
	}
	info, err := w.f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect log %s: %w", w.path, err)
	}
	if info.Size() < w.off {
		w.off, w.partial, w.skip = 0, nil, false
	}
	n := min(info.Size()-w.off, maxLogRead)
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	read, err := w.f.ReadAt(buf, w.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read log %s: %w", w.path, err)
	}
	w.off += int64(read)
	return w.scan(buf[:read]), nil
}

// open starts reading a log file that is not the one open before.
func (w *Watcher) open(cur os.FileInfo) error {
	w.Close()
	// O_NONBLOCK: a FIFO swapped in after the check must not block open.
	f, err := os.OpenFile(w.path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open log %s: %w", w.path, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, cur) {
		f.Close()
		return nil // it changed under us; look again next poll
	}
	w.f, w.info, w.partial, w.skip = f, info, nil, false
	w.off = 0
	if w.continues(f, info) {
		w.off = w.baseSize
	}
	return nil
}

func (w *Watcher) scan(data []byte) []string {
	var lines []string
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if !w.skip {
				w.partial = append(w.partial, data...)
				if len(w.partial) > maxLogLine {
					w.partial, w.skip = nil, true
				}
			}
			return lines
		}
		line := append(w.partial, data[:i]...)
		data = data[i+1:]
		w.partial = nil
		if w.skip {
			w.skip = false
			continue
		}
		if len(line) > maxLogLine {
			continue
		}
		text := string(bytes.TrimRight(line, "\r"))
		if failureHints.MatchString(text) {
			w.Hints = append(w.Hints, text)
			if len(w.Hints) > maxHints {
				w.Hints = w.Hints[1:]
			}
		}
		lines = append(lines, text)
	}
	return lines
}
