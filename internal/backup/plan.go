package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// kind is what an archive entry holds.
type kind int

const (
	kindDir kind = iota
	kindFile
	kindLink
)

// entry is one thing to put in an archive.
type entry struct {
	// Name is the slash-separated path inside the archive.
	Name string
	Kind kind
	// Src is the file to read for a kindFile; for a followed link it is
	// the link's target.
	Src  string
	Info fs.FileInfo
	// Target is the link text of a kindLink.
	Target string
}

// policy says how a walk treats symbolic links and what it leaves out.
type policy struct {
	// Follow stores what a link points to instead of the link.
	Follow bool
	// Allowed are canonical directories a followed link may point into;
	// a target elsewhere is a violation unless AllowExternal.
	Allowed       []string
	AllowExternal bool
	// Exclude are directories (and files) that never enter the archive:
	// the archive's own directory, staging and the pending marker.
	Exclude []fs.FileInfo
	// ExcludeNames are file names skipped wherever they appear at the top
	// of the walked tree.
	ExcludeNames []string
}

// plan is the result of a walk.
type plan struct {
	Entries  []entry
	Warnings []string
	// External lists followed links whose targets lie outside the allowed
	// directories: "link -> target".
	External []string
}

var (
	// ErrExternalLinks is a followed link that leaves the managed tree.
	ErrExternalLinks = errors.New("backup: symbolic links point outside the server")
	// ErrLinkCycle is a followed link that leads back into the directory
	// being archived.
	ErrLinkCycle = errors.New("backup: symbolic links form a cycle")
)

// walker builds a plan.
type walker struct {
	p     policy
	plan  *plan
	stack []fs.FileInfo // directories being descended, for cycle detection
}

// buildPlan walks root (a real directory) and names it top inside the
// archive, in a deterministic order: directories first-seen, entries
// sorted by name, as zip -r lists them.
func buildPlan(root, top string, p policy) (*plan, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	w := &walker{p: p, plan: &plan{}}
	if err := w.dir(root, top, info, true); err != nil {
		return nil, err
	}
	if len(w.plan.External) > 0 && !p.AllowExternal {
		sort.Strings(w.plan.External)
		return w.plan, fmt.Errorf("%w; refusing to archive their targets (they would put data from outside the server in the archive): %s; remove the links, or run with --allow-external-links after reviewing this list",
			ErrExternalLinks, strings.Join(w.plan.External, "; "))
	}
	return w.plan, nil
}

func (w *walker) excluded(info fs.FileInfo) bool {
	for _, ex := range w.p.Exclude {
		if os.SameFile(info, ex) {
			return true
		}
	}
	return false
}

func (w *walker) dir(fsPath, name string, info fs.FileInfo, top bool) error {
	for _, in := range w.stack {
		if os.SameFile(in, info) {
			return fmt.Errorf("%w: %s leads back to a directory already being archived", ErrLinkCycle, fsPath)
		}
	}
	w.stack = append(w.stack, info)
	defer func() { w.stack = w.stack[:len(w.stack)-1] }()
	w.plan.Entries = append(w.plan.Entries, entry{Name: name + "/", Kind: kindDir, Src: fsPath, Info: info})
	entries, err := os.ReadDir(fsPath)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if top && contains(w.p.ExcludeNames, e.Name()) {
			continue
		}
		if err := w.child(filepath.Join(fsPath, e.Name()), path.Join(name, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (w *walker) child(fsPath, name string) error {
	info, err := os.Lstat(fsPath)
	if errors.Is(err, fs.ErrNotExist) {
		w.plan.Warnings = append(w.plan.Warnings, fmt.Sprintf("%s disappeared while the backup was prepared and is not in it", fsPath))
		return nil
	}
	if err != nil {
		return err
	}
	if w.excluded(info) {
		return nil
	}
	switch mode := info.Mode(); {
	case mode.IsDir():
		return w.dir(fsPath, name, info, false)
	case mode.IsRegular():
		w.plan.Entries = append(w.plan.Entries, entry{Name: name, Kind: kindFile, Src: fsPath, Info: info})
		return nil
	case mode&fs.ModeSymlink != 0:
		return w.link(fsPath, name, info)
	default:
		w.plan.Warnings = append(w.plan.Warnings, fmt.Sprintf("%s is a %s and is not in the backup", fsPath, kindName(mode)))
		return nil
	}
}

func kindName(m fs.FileMode) string {
	switch {
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

func (w *walker) link(fsPath, name string, info fs.FileInfo) error {
	target, err := os.Readlink(fsPath)
	if err != nil {
		return err
	}
	store := entry{Name: name, Kind: kindLink, Src: fsPath, Info: info, Target: target}
	if !w.p.Follow {
		w.plan.Entries = append(w.plan.Entries, store)
		return nil
	}
	real, err := filepath.EvalSymlinks(fsPath)
	if err != nil {
		w.plan.Warnings = append(w.plan.Warnings, fmt.Sprintf("%s points to %s, which does not exist; the link is stored as a link", fsPath, target))
		w.plan.Entries = append(w.plan.Entries, store)
		return nil
	}
	tinfo, err := os.Stat(real)
	if err != nil {
		return err
	}
	if w.excluded(tinfo) {
		return nil
	}
	if !w.allowed(real) {
		w.plan.External = append(w.plan.External, fsPath+" -> "+real)
		if !w.p.AllowExternal {
			return nil
		}
	}
	switch {
	case tinfo.IsDir():
		return w.dir(real, name, tinfo, false)
	case tinfo.Mode().IsRegular():
		w.plan.Entries = append(w.plan.Entries, entry{Name: name, Kind: kindFile, Src: real, Info: tinfo})
		return nil
	default:
		w.plan.Warnings = append(w.plan.Warnings, fmt.Sprintf("%s points to a %s and is not in the backup", fsPath, kindName(tinfo.Mode())))
		return nil
	}
}

func (w *walker) allowed(real string) bool {
	for _, dir := range w.p.Allowed {
		if real == dir || strings.HasPrefix(real, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
