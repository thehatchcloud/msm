package worlds

// RAM-backed worlds.
//
// A world flagged for RAM (the flag file in its directory) is served from a
// copy in RAMDISK_STORAGE_PATH/<server>/<world> while its server runs, and
// the directory on disk stays the durable copy. The manager never creates or
// mounts a RAM disk: the administrator prepares one (tmpfs on Linux, a
// hdiutil ram:// volume on macOS) and RAMDISK_STORAGE_PATH must be inside
// it. A path on an ordinary disk is refused, not accepted under a RAM name.
//
// Two fingerprints decide what may be copied which way. The manifest in the
// RAM copy records the fingerprint of each side when they last matched (the
// copy to RAM, or the last successful write to disk). RAM is written to disk
// only when the disk copy still has that fingerprint, and the disk copy
// replaces the RAM copy only when the RAM copy still has its own. A side that
// changed since the last match is newer than the baseline; when both changed
// the manager changes neither, serves the disk copy and says so.
//
// Disk is only ever changed by building a complete staged copy beside the
// world, checking it against the RAM copy, and swapping it in with one
// atomic exchange, so a full disk, an interrupted copy or a crash leaves the
// previous durable copy in place.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thehatchcloud/msm/internal/atomicfile"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

var (
	// ErrRAMConfig is a RAM setting that does not allow RAM worlds.
	ErrRAMConfig = errors.New("worlds: RAM worlds are not configured")
	// ErrRAMUnsupported is a RAM path that is not on a RAM-backed volume.
	ErrRAMUnsupported = errors.New("worlds: RAM-backed storage is not available")
	// ErrRAMSpace is a filesystem without room for a copy.
	ErrRAMSpace = errors.New("worlds: not enough space")
	// ErrRAMConflict is a world changed on both sides since they matched.
	ErrRAMConflict = errors.New("worlds: RAM and disk copies both changed")
	// ErrRAMUnmanaged is a RAM copy this manager did not make.
	ErrRAMUnmanaged = errors.New("worlds: RAM copy has no manifest")
)

const (
	ramStagePrefix = ".msm-ram-stage-"
	ramOldPrefix   = ".msm-ram-old-"
	syncStage      = stagePrefix + "sync-"
	syncOld        = stagePrefix + "old-"
)

// runProgram runs an external program and returns its output. Only the
// macOS RAM-disk check uses it (hdiutil).
type runProgram func(ctx context.Context, name string, args ...string) ([]byte, error)

// ramManifest is stored in the RAM copy.
type ramManifest struct {
	Version int    `json:"version"`
	Server  string `json:"server"`
	World   string `json:"world"`
	// DiskFingerprint and RAMFingerprint are the two sides when they last
	// matched.
	DiskFingerprint string    `json:"disk_fingerprint"`
	RAMFingerprint  string    `json:"ram_fingerprint"`
	SyncedAt        time.Time `json:"synced_at"`
}

func readManifest(copyDir string) (ramManifest, bool, error) {
	data, err := os.ReadFile(filepath.Join(copyDir, ramManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return ramManifest{}, false, nil
	}
	if err != nil {
		return ramManifest{}, false, err
	}
	var mf ramManifest
	if err := json.Unmarshal(data, &mf); err != nil || mf.Version != 1 {
		return ramManifest{}, false, fmt.Errorf("%w: %s is not readable", ErrRAMUnmanaged, filepath.Join(copyDir, ramManifestName))
	}
	return mf, true, nil
}

func writeManifest(copyDir string, mf ramManifest, owner identity.Identity, euid int) error {
	mf.Version = 1
	data, err := json.Marshal(mf)
	if err != nil {
		return err
	}
	path := filepath.Join(copyDir, ramManifestName)
	if err := atomicfile.Write(path, data, 0o644); err != nil {
		return err
	}
	if euid == 0 && owner.UID != 0 {
		return os.Lchown(path, owner.UID, owner.GID)
	}
	return nil
}

// ramRun is one server's context for RAM operations.
type ramRun struct {
	m     *Manager
	l     *layout
	owner identity.Identity
	euid  int
	root  string // RAMDISK_STORAGE_PATH, validated
	dir   string // root/<server>
}

func (r *ramRun) copyDir(w World) string { return filepath.Join(r.dir, w.Name) }

func (r *ramRun) copyOpts(skip ...string) copyOptions {
	o := copyOptions{Skip: map[string]bool{}}
	for _, s := range skip {
		o.Skip[s] = true
	}
	if r.euid == 0 && r.owner.UID != 0 {
		owner := r.owner
		o.Owner = &owner
	}
	return o
}

// ramSetting explains why RAM worlds are off, or returns their path.
func (m *Manager) ramSetting() (string, error) {
	g := m.cfg.Servers.Global()
	if g == nil {
		return "", fmt.Errorf("%w: there is no msm.conf, so RAMDISK_STORAGE_ENABLED is off; set RAMDISK_STORAGE_ENABLED=\"true\" and RAMDISK_STORAGE_PATH to a path inside a RAM disk you prepared", ErrRAMConfig)
	}
	if v, ok := g.Lookup("RAMDISK_STORAGE_ENABLED"); !ok || v != "true" {
		return "", fmt.Errorf("%w: RAMDISK_STORAGE_ENABLED is not \"true\"; RAM worlds are off by default and stay off until a RAM disk is prepared and the setting enabled", ErrRAMConfig)
	}
	p := defaultRAMPath
	if v, ok := g.Lookup("RAMDISK_STORAGE_PATH"); ok {
		p = v
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w: RAMDISK_STORAGE_PATH %q must be an absolute path", ErrRAMConfig, p)
	}
	return filepath.Clean(p), nil
}

// prepareRAM validates the RAM path and returns a context for the server:
// the path is inside a RAM-backed filesystem that exists, and the server's
// directory in it is there and writable.
func (m *Manager) prepareRAM(s *legacyconf.ServerSettings, l *layout) (*ramRun, error) {
	root, err := m.ramSetting()
	if err != nil {
		return nil, err
	}
	owner, err := m.cfg.Servers.Owner(s)
	if err != nil {
		return nil, err
	}
	euid := m.cfg.Servers.EUID()
	if euid != 0 && euid != owner.UID {
		return nil, fmt.Errorf("%w: server %q belongs to %s (uid %d); run as that user or as root",
			identity.ErrPrivilegeRequired, s.Name, owner.Username, owner.UID)
	}
	// The mount must already exist: only directories inside it are made.
	anc := root
	for {
		if _, err := os.Stat(anc); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("worlds: inspect %s: %w", anc, err)
		}
		next := filepath.Dir(anc)
		if next == anc {
			return nil, fmt.Errorf("%w: no part of RAMDISK_STORAGE_PATH %s exists", ErrRAMUnsupported, root)
		}
		anc = next
	}
	if err := m.ramCheck(anc); err != nil {
		return nil, fmt.Errorf("%w: RAMDISK_STORAGE_PATH %s: %v; this manager does not create or mount RAM disks, prepare one and point RAMDISK_STORAGE_PATH into it (docs/ram-worlds.md)", ErrRAMUnsupported, root, err)
	}
	if info, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		if err := m.ops.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("worlds: create %s: %w", root, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("worlds: inspect %s: %w", root, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%w: RAMDISK_STORAGE_PATH %s is not a directory", ErrRAMConfig, root)
	}
	if err := m.ramCheck(root); err != nil {
		return nil, fmt.Errorf("%w: RAMDISK_STORAGE_PATH %s: %v", ErrRAMUnsupported, root, err)
	}
	r := &ramRun{m: m, l: l, owner: owner, euid: euid, root: root, dir: filepath.Join(root, s.Name)}
	if err := m.ops.MkdirAll(r.dir, 0o755); err != nil {
		return nil, fmt.Errorf("worlds: create %s: %w", r.dir, err)
	}
	if euid == 0 && owner.UID != 0 {
		if err := os.Lchown(r.dir, owner.UID, owner.GID); err != nil {
			return nil, fmt.Errorf("worlds: hand %s to %s: %w", r.dir, owner.Username, err)
		}
	}
	// Leftovers of a copy that was cut short are ours (the server's lock is
	// held) and RAM is disposable.
	r.sweep()
	return r, nil
}

func (r *ramRun) sweep() {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ramStagePrefix) || strings.HasPrefix(e.Name(), ramOldPrefix) {
			_ = r.m.ops.RemoveAll(filepath.Join(r.dir, e.Name()))
		}
	}
}

// margin is the room kept beyond the bytes copied.
func margin(n int64) uint64 { return uint64(max(8<<20, n/20)) }

func (m *Manager) needSpace(dir string, bytes int64, what string) error {
	free, err := m.free(dir)
	if err != nil {
		return fmt.Errorf("worlds: check free space on %s: %w", dir, err)
	}
	if need := uint64(bytes) + margin(bytes); free < need {
		return fmt.Errorf("%w on %s for %s: %d bytes needed, %d free", ErrRAMSpace, dir, what, need, free)
	}
	return nil
}

func (r *ramRun) diskScan(w World) (treeInfo, error) {
	return scanTree(w.Dir, map[string]bool{r.l.flag: true})
}

func (r *ramRun) ramScan(w World) (treeInfo, error) {
	return scanTree(r.copyDir(w), map[string]bool{ramManifestName: true})
}

// seed makes the RAM copy of w from its disk directory: copied beside the
// final place, fingerprinted, given its manifest and renamed into place.
func (r *ramRun) seed(ctx context.Context, w World) error {
	disk, err := r.diskScan(w)
	if err != nil {
		return fmt.Errorf("worlds: read %s: %w", w.Dir, err)
	}
	if err := r.m.needSpace(r.dir, disk.Bytes, fmt.Sprintf("the RAM copy of world %q", w.Name)); err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	stage := filepath.Join(r.dir, ramStagePrefix+w.Name+"-"+suffix)
	fail := func(err error) error {
		_ = r.m.ops.RemoveAll(stage)
		if isNoSpace(err) {
			return fmt.Errorf("%w: copying world %q to RAM: %v; the disk copy is unchanged", ErrRAMSpace, w.Name, err)
		}
		return fmt.Errorf("worlds: copy world %q to RAM: %w; the disk copy is unchanged", w.Name, err)
	}
	if err := r.m.ops.CopyWorld(ctx, w.Dir, stage, r.copyOpts(r.l.flag)); err != nil {
		return fail(err)
	}
	// The world must not have changed while it was copied.
	if again, err := r.diskScan(w); err != nil || again.Fingerprint != disk.Fingerprint {
		return fail(fmt.Errorf("%s changed while it was being copied", w.Dir))
	}
	ram, err := scanTree(stage, nil)
	if err != nil || ram.Fingerprint != disk.Fingerprint {
		return fail(errors.New("the copy does not match the original"))
	}
	if err := writeManifest(stage, ramManifest{Server: r.l.s.Name, World: w.Name,
		DiskFingerprint: disk.Fingerprint, RAMFingerprint: ram.Fingerprint, SyncedAt: time.Now().UTC()}, r.owner, r.euid); err != nil {
		return fail(err)
	}
	if err := r.m.ops.Rename(stage, r.copyDir(w)); err != nil {
		return fail(err)
	}
	return nil
}

// reconcile makes the RAM copy of w ready to serve at server start: made if
// absent, kept if it is current or holds changes not yet on disk, replaced
// from disk if only the disk copy changed. It fails, touching neither side,
// when both changed or the RAM copy is not one this manager made.
func (r *ramRun) reconcile(ctx context.Context, w World, out Output) error {
	cp := r.copyDir(w)
	if _, err := os.Lstat(cp); errors.Is(err, fs.ErrNotExist) {
		out.printf("Copying world %q to RAM...", w.Name)
		return r.seed(ctx, w)
	} else if err != nil {
		return fmt.Errorf("worlds: inspect %s: %w", cp, err)
	}
	mf, ok, err := readManifest(cp)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s was not made by this manager, so it is not used or replaced; move it aside once you have checked it holds nothing you need", ErrRAMUnmanaged, cp)
	}
	disk, err := r.diskScan(w)
	if err != nil {
		return fmt.Errorf("worlds: read %s: %w", w.Dir, err)
	}
	ram, err := r.ramScan(w)
	if err != nil {
		return fmt.Errorf("worlds: read %s: %w", cp, err)
	}
	diskChanged, ramChanged := disk.Fingerprint != mf.DiskFingerprint, ram.Fingerprint != mf.RAMFingerprint
	switch {
	case !diskChanged:
		if ramChanged {
			out.printf("World %q: the RAM copy holds changes not yet written to disk; keeping it.", w.Name)
		}
		return nil
	case ramChanged:
		return fmt.Errorf("%w: world %q changed both in %s and in %s since they last matched; neither was changed, the server will use the disk copy; see docs/ram-worlds.md to choose which to keep", ErrRAMConflict, w.Name, w.Dir, cp)
	}
	// Only the disk copy changed: the RAM copy is out of date.
	out.printf("World %q changed on disk since it was copied to RAM; copying it again...", w.Name)
	trash := filepath.Join(r.dir, ramOldPrefix+w.Name)
	_ = r.m.ops.RemoveAll(trash)
	if err := r.m.ops.Rename(cp, trash); err != nil {
		return fmt.Errorf("worlds: set aside the old RAM copy %s: %w", cp, err)
	}
	if err := r.seed(ctx, w); err != nil {
		// The old copy matched an earlier state of the disk copy, which
		// is still on disk; put it back for the next attempt.
		_ = r.m.ops.Rename(trash, cp)
		return err
	}
	_ = r.m.ops.RemoveAll(trash)
	return nil
}

// toDisk writes the RAM copy of w to its directory on disk. live means the
// server is running from the RAM copy. It reports whether disk changed.
func (r *ramRun) toDisk(ctx context.Context, w World, live bool, out Output) (bool, error) {
	cp := r.copyDir(w)
	mf, ok, err := readManifest(cp)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("%w: %s was not made by this manager, so it was not written to disk", ErrRAMUnmanaged, cp)
	}
	if live {
		if link, err := os.Readlink(filepath.Join(r.l.s.Dir, w.Name)); err != nil || link != cp {
			out.warnf("Warning: world %q is not being served from RAM, so there is nothing to write", w.Name)
			return false, nil
		}
	}
	disk, err := r.diskScan(w)
	if err != nil {
		return false, fmt.Errorf("worlds: read %s: %w", w.Dir, err)
	}
	ram, err := r.ramScan(w)
	if err != nil {
		return false, fmt.Errorf("worlds: read %s: %w", cp, err)
	}
	diskChanged, ramChanged := disk.Fingerprint != mf.DiskFingerprint, ram.Fingerprint != mf.RAMFingerprint
	switch {
	case !ramChanged:
		return false, nil
	case diskChanged:
		return false, fmt.Errorf("%w: world %q changed both in %s and in %s since they last matched, so nothing was written; see docs/ram-worlds.md", ErrRAMConflict, w.Name, w.Dir, cp)
	}
	parent := filepath.Dir(w.Dir)
	if err := r.m.needSpace(parent, ram.Bytes, fmt.Sprintf("the disk copy of world %q", w.Name)); err != nil {
		return false, fmt.Errorf("%w; the disk copy keeps its last synchronized state", err)
	}
	r.cleanSyncLeftovers(parent, w.Name)
	suffix, err := randomSuffix()
	if err != nil {
		return false, err
	}
	stage := filepath.Join(parent, syncStage+w.Name+"-"+suffix)
	fail := func(err error) (bool, error) {
		_ = r.m.ops.RemoveAll(stage)
		if isNoSpace(err) {
			return false, fmt.Errorf("%w: writing world %q to %s: %v; the disk copy keeps its last synchronized state", ErrRAMSpace, w.Name, parent, err)
		}
		return false, fmt.Errorf("worlds: write world %q to disk: %w; the disk copy keeps its last synchronized state", w.Name, err)
	}
	if err := r.m.ops.CopyWorld(ctx, cp, stage, r.copyOpts(ramManifestName)); err != nil {
		return fail(err)
	}
	// The flag stays with the world on disk.
	if _, err := os.Lstat(filepath.Join(w.Dir, r.l.flag)); err == nil {
		if err := r.touchFlag(stage); err != nil {
			return fail(err)
		}
	}
	if again, err := r.ramScan(w); err != nil || again.Fingerprint != ram.Fingerprint {
		return fail(fmt.Errorf("%s changed while it was being copied", cp))
	}
	staged, err := scanTree(stage, map[string]bool{r.l.flag: true})
	if err != nil || staged.Fingerprint != ram.Fingerprint {
		return fail(errors.New("the copy does not match the RAM copy"))
	}
	if err := r.publish(stage, w.Dir, suffix); err != nil {
		return fail(err)
	}
	syncDir(parent)
	now, err := r.diskScan(w)
	if err != nil {
		return true, fmt.Errorf("worlds: world %q was written to disk, but reading it back failed: %w", w.Name, err)
	}
	if err := writeManifest(cp, ramManifest{Server: r.l.s.Name, World: w.Name,
		DiskFingerprint: now.Fingerprint, RAMFingerprint: ram.Fingerprint, SyncedAt: time.Now().UTC()}, r.owner, r.euid); err != nil {
		return true, fmt.Errorf("worlds: world %q was written to disk, but its RAM manifest was not updated: %w; the next synchronization will refuse it as a conflict, see docs/ram-worlds.md", w.Name, err)
	}
	return true, nil
}

func (r *ramRun) touchFlag(dir string) error {
	path := filepath.Join(dir, r.l.flag)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if r.euid == 0 && r.owner.UID != 0 {
		return os.Lchown(path, r.owner.UID, r.owner.GID)
	}
	return nil
}

// publish puts the complete directory stage in place of dst. The swap is one
// atomic exchange; a filesystem that cannot exchange gets two renames, and
// the previous directory stays beside the world, named, if the second fails.
func (r *ramRun) publish(stage, dst, suffix string) error {
	ok, err := r.m.ops.Exchange(stage, dst)
	if err != nil {
		return err
	}
	if ok {
		// stage now holds the previous durable copy.
		if err := r.m.ops.RemoveAll(stage); err != nil {
			return nil // reported by the next list as a leftover; the swap is done
		}
		return nil
	}
	old := filepath.Join(filepath.Dir(dst), syncOld+filepath.Base(dst)+"-"+suffix)
	if err := r.m.ops.Rename(dst, old); err != nil {
		return err
	}
	if err := r.m.ops.Rename(stage, dst); err != nil {
		if undo := r.m.ops.Rename(old, dst); undo != nil {
			return fmt.Errorf("%w; the previous copy is at %s: move it back to %s", err, old, dst)
		}
		return err
	}
	_ = r.m.ops.RemoveAll(old)
	return nil
}

func (r *ramRun) cleanSyncLeftovers(parent, world string) {
	for _, prefix := range []string{syncStage + world + "-", syncOld + world + "-"} {
		matches, _ := filepath.Glob(filepath.Join(parent, prefix+"*"))
		for _, p := range matches {
			if info, err := os.Lstat(p); err == nil && info.IsDir() {
				_ = r.m.ops.RemoveAll(p)
			}
		}
	}
}

// removeCopy deletes the RAM copy of w. It refuses anything that is not
// the world's own directory inside the server's RAM directory.
func (r *ramRun) removeCopy(w World) error {
	cp := r.copyDir(w)
	if !within(cp, r.dir) {
		return fmt.Errorf("worlds: refusing to remove %s: it is not inside %s", cp, r.dir)
	}
	info, err := os.Lstat(cp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("worlds: %s is not a directory", cp)
	}
	return r.m.ops.RemoveAll(cp)
}

// SyncToDisk writes the RAM copy of each RAM world of a server to disk
// (the core of msm <server> worlds todisk, shutdown and backup). The caller
// holds the server's lock. live means the server is running with saving off
// and its worlds are served from RAM; otherwise the server must be stopped.
// Each world is attempted on its own and the error names every failure.
func (m *Manager) SyncToDisk(ctx context.Context, s *legacyconf.ServerSettings, live bool, out Output) error {
	if _, err := m.ramSetting(); err != nil {
		return nil // RAM worlds are off: no RAM copy is in use
	}
	l, err := m.load(s)
	if err != nil {
		return err
	}
	var todo []World
	for _, w := range l.worlds {
		if w.Active && w.InRAM && !l.ambiguousNames[w.Name] {
			todo = append(todo, w)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	r, err := m.prepareRAM(s, l)
	if err != nil {
		for _, w := range todo {
			if _, statErr := os.Lstat(filepath.Join(m.ramRootOrEmpty(), s.Name, w.Name)); statErr == nil {
				return fmt.Errorf("worlds: cannot write the RAM worlds of %q to disk: %w", s.Name, err)
			}
		}
		if live {
			return fmt.Errorf("worlds: server %q runs with RAM worlds, but RAM is not usable: %w", s.Name, err)
		}
		out.warnf("Warning: no RAM copy of the worlds of %q exists (%v); the copies on disk, from the last synchronization, are in use", s.Name, err)
		return nil
	}
	var failed []string
	for _, w := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Lstat(r.copyDir(w)); errors.Is(err, fs.ErrNotExist) {
			if live {
				failed = append(failed, fmt.Sprintf("%s: the RAM copy %s is missing while the server runs", w.Name, r.copyDir(w)))
				continue
			}
			out.warnf("Warning: world %q has no RAM copy; its copy on disk, from the last synchronization, is in use", w.Name)
			continue
		}
		changed, err := r.toDisk(ctx, w, live, out)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("worlds: interrupted while writing world %q to disk (its RAM copy is intact, the disk copy is as it was): %w", w.Name, cerr)
			}
			out.warnf("Error: %v", err)
			failed = append(failed, w.Name)
			continue
		}
		if changed {
			out.printf("Synchronized world %q from RAM to disk.", w.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("worlds: %d of %d RAM worlds of %q could not be written to disk (their RAM copies are intact): %s", len(failed), len(todo), s.Name, strings.Join(failed, ", "))
	}
	return nil
}

func (m *Manager) ramRootOrEmpty() string {
	p, err := m.ramSetting()
	if err != nil {
		return ""
	}
	return p
}

// ToggleRAM flips a world's RAM flag (msm <server> worlds ram <world>). The
// server must be stopped. Switching on copies the world to RAM first and
// sets the flag only when the copy is complete; switching off writes the RAM
// copy to disk first and removes it only when the disk copy is durable.
func (m *Manager) ToggleRAM(ctx context.Context, name, world string, out Output) error {
	x, err := m.open(ctx, name)
	if err != nil {
		return err
	}
	defer x.release()
	if err := m.cfg.Servers.ActAsOwner(x.owner); err != nil {
		return err
	}
	l, err := m.load(x.s)
	if err != nil {
		return err
	}
	w, err := l.find(world)
	if err != nil {
		return err
	}
	if !w.Active {
		return fmt.Errorf("worlds: world %q is not active; activate it with \"worlds on\" before changing its RAM status", w.Name)
	}
	flag := filepath.Join(w.Dir, l.flag)
	if w.InRAM {
		return m.ramOff(ctx, x.s, l, w, flag, out)
	}
	r, err := m.prepareRAM(x.s, l)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(r.copyDir(w)); err == nil {
		return fmt.Errorf("%w: %s already exists although world %q is not flagged; it was not used or replaced, move it aside once you have checked it", ErrRAMUnmanaged, r.copyDir(w), w.Name)
	}
	out.printf("Copying world %q to RAM...", w.Name)
	if err := r.seed(ctx, w); err != nil {
		return err
	}
	out.printf("Adding RAM flag to world %q...", w.Name)
	if err := r.touchFlag(w.Dir); err != nil {
		_ = r.removeCopy(w)
		return fmt.Errorf("worlds: flag world %q: %w; the RAM copy was removed", w.Name, err)
	}
	out.printf("Changes will only take effect after server is restarted.")
	return nil
}

func (m *Manager) ramOff(ctx context.Context, s *legacyconf.ServerSettings, l *layout, w World, flag string, out Output) error {
	var r *ramRun
	if _, err := m.ramSetting(); err == nil {
		var perr error
		if r, perr = m.prepareRAM(s, l); perr != nil {
			// A copy that cannot be reached cannot be proven synchronized.
			if _, statErr := os.Lstat(filepath.Join(m.ramRootOrEmpty(), s.Name, w.Name)); statErr == nil {
				return perr
			}
			r = nil
		}
	}
	if r != nil {
		if _, err := os.Lstat(r.copyDir(w)); err == nil {
			out.printf("Synchronising world %q to disk...", w.Name)
			if _, err := r.toDisk(ctx, w, false, out); err != nil {
				return err
			}
		}
	}
	out.printf("Removing RAM flag from world %q...", w.Name)
	if err := os.Remove(flag); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("worlds: remove the flag of world %q: %w; its RAM copy was kept", w.Name, err)
	}
	if r != nil {
		out.printf("Removing world %q from RAM...", w.Name)
		if err := r.removeCopy(w); err != nil {
			out.warnf("Warning: the RAM copy %s could not be removed: %v", r.copyDir(w), err)
		}
	}
	out.printf("Changes will only take effect after server is restarted.")
	return nil
}

// ramServed prepares the RAM copy of each flagged world at server start and
// returns the link target for it, or "" with the reason when the world must
// be linked to its disk copy.
type ramServed struct {
	run    *ramRun
	failed error
	tried  bool
}

func (m *Manager) ramTarget(ctx context.Context, rs *ramServed, l *layout, w World, out Output) string {
	if _, err := m.ramSetting(); err != nil {
		out.warnf("Warning: world %q is flagged to live in RAM, but %v; it is linked to its copy on disk", w.Name, strings.TrimPrefix(err.Error(), ErrRAMConfig.Error()+": "))
		return w.Dir
	}
	if !rs.tried {
		rs.tried = true
		rs.run, rs.failed = m.prepareRAM(l.s, l)
	}
	if rs.failed != nil {
		out.warnf("Warning: world %q is flagged to live in RAM, but %v; it is linked to its copy on disk", w.Name, rs.failed)
		return w.Dir
	}
	if err := rs.run.reconcile(ctx, w, out); err != nil {
		out.warnf("Warning: world %q is flagged to live in RAM, but its RAM copy cannot be used: %v; it is linked to its copy on disk", w.Name, err)
		return w.Dir
	}
	return rs.run.copyDir(w)
}

// ErrSaveRefused is a server that would not change its saving state, as
// when saving is already off.
var ErrSaveRefused = errors.New("worlds: the server refused the command")

// Saver controls a running server's saving; the CLI backs it with the
// console manager. Every call is made with the server's lock already held.
type Saver interface {
	// Running reports whether the server's console is reachable.
	Running(ctx context.Context, set *legacyconf.ServerSettings, out Output) (bool, error)
	// SaveOff disables saving, SaveAll saves and flushes the worlds, SaveOn
	// enables saving. A refusal wraps ErrSaveRefused.
	SaveOff(ctx context.Context, set *legacyconf.ServerSettings, out Output) error
	SaveAll(ctx context.Context, set *legacyconf.ServerSettings, out Output) error
	SaveOn(ctx context.Context, set *legacyconf.ServerSettings, out Output) error
}

// saveWindow bounds how long saving stays off for a synchronization.
const saveWindow = 10 * time.Minute

// ToDisk writes a server's RAM worlds to disk (msm <server> worlds todisk,
// which cron runs periodically). A running server has saving turned off and
// its worlds saved first, and saving is turned back on afterwards, on every
// error and on interruption; a stopped server is proven stopped.
func (m *Manager) ToDisk(ctx context.Context, name string, out Output) (err error) {
	lock, s, err := m.cfg.Servers.LockServer(name)
	if err != nil {
		return err
	}
	defer lock.Release()
	if _, cfgErr := m.ramSetting(); cfgErr != nil {
		out.printf("RAM worlds are off for %q; nothing to write to disk.", name)
		return nil
	}
	l, err := m.load(s)
	if err != nil {
		return err
	}
	anyRAM := false
	for _, w := range l.worlds {
		if w.Active && w.InRAM && !l.ambiguousNames[w.Name] {
			anyRAM = true
		}
	}
	if !anyRAM {
		out.printf("Server %q has no worlds in RAM.", name)
		return nil
	}
	running := false
	if m.cfg.Saver != nil {
		if running, err = m.cfg.Saver.Running(ctx, s, out); err != nil {
			return err
		}
	}
	if !running {
		if err := servers.RequireStopped(name, m.cfg.Servers.Probe(ctx, s)); err != nil {
			return err
		}
		out.printf("Synchronising worlds in RAM to disk...")
		if err := m.SyncToDisk(ctx, s, false, out); err != nil {
			return err
		}
		out.printf("Done.")
		return nil
	}
	window, cancel := context.WithTimeout(ctx, saveWindow)
	defer cancel()
	saving := true
	defer func() {
		if !saving {
			return
		}
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer rcancel()
		if rerr := m.cfg.Saver.SaveOn(rctx, s, out); rerr != nil {
			err = errors.Join(err, fmt.Errorf("worlds: saving could not be turned back on for server %q: %v; run \"save-on\" on its console", name, rerr))
		}
	}()
	err = m.cfg.Saver.SaveOff(window, s, out)
	if errors.Is(err, ErrSaveRefused) {
		// Saving was already off; it is not ours to turn back on.
		saving = false
		return fmt.Errorf("the server refused to disable saving, so it was left as it was: %w", err)
	}
	if err != nil {
		return fmt.Errorf("disable saving before writing RAM worlds: %w", err)
	}
	if err := m.cfg.Saver.SaveAll(window, s, out); err != nil {
		return fmt.Errorf("save the server before writing RAM worlds: %w", err)
	}
	out.printf("Synchronising worlds in RAM to disk...")
	if err := m.SyncToDisk(window, s, true, out); err != nil {
		return err
	}
	out.printf("Done.")
	return nil
}
