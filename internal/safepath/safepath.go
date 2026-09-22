// Package safepath validates untrusted names and paths before they touch the
// filesystem: the <name> grammar from the compatibility contract, root
// containment against traversal and symlink escape, sanity of externally
// configured storage roots, and case-insensitive collisions on filesystems
// (or administrators) that do not distinguish case.
package safepath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// reservedNames mirrors the compatibility contract's <name> grammar: names
// that would collide with fixed command-line tokens.
var reservedNames = map[string]bool{
	"start": true, "stop": true, "restart": true, "version": true,
	"server": true, "jargroup": true, "all": true, "config": true,
	"update": true, "help": true,
}

// ValidateName reports whether name is safe to use as a server, world, or
// JAR group name: ASCII letters, digits, underscore and dash only, never
// empty, never one of the reserved command tokens, and never starting with
// "--" (which the dispatcher's grammar treats as the start of a flag run).
func ValidateName(name string) error {
	if name == "" {
		return errors.New("safepath: name must not be empty")
	}
	if strings.HasPrefix(name, "--") {
		return fmt.Errorf("safepath: %q looks like a flag, not a name", name)
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("safepath: %q contains characters other than letters, digits, '_' and '-'", name)
	}
	if reservedNames[strings.ToLower(name)] {
		return fmt.Errorf("safepath: %q is a reserved command name", name)
	}
	return nil
}

// maxSymlinkHops bounds symlink resolution while walking into root, so a
// symlink cycle fails with a clear error instead of looping forever.
const maxSymlinkHops = 40

// Contain resolves rel against root and guarantees the result stays inside
// root. It rejects an absolute rel, any ".." segment that would climb above
// root, and any intermediate symlink (already present under root) that
// points outside root. rel need not exist yet: Contain validates every
// ancestor segment that does exist and simply joins the rest.
//
// The returned path has existing symlink ancestors resolved, so callers
// operate on the real on-disk location rather than a name that merely
// appears to be inside root.
func Contain(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("safepath: %q must be a relative path", rel)
	}
	cleanRel := filepath.Clean(rel)
	if cleanRel == "." {
		cleanRel = ""
	}
	if cleanRel == ".." || strings.HasPrefix(cleanRel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("safepath: %q escapes its root", rel)
	}

	canonicalRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("safepath: resolve root %s: %w", root, err)
	}
	if resolved, err := filepath.EvalSymlinks(canonicalRoot); err == nil {
		canonicalRoot = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("safepath: resolve root %s: %w", root, err)
	}

	current := canonicalRoot
	hops := 0
	if cleanRel != "" {
		for _, segment := range strings.Split(cleanRel, string(filepath.Separator)) {
			next := filepath.Join(current, segment)
			resolved, err := resolveSymlinkChain(next, canonicalRoot, &hops)
			if err != nil {
				return "", err
			}
			current = resolved
		}
	}
	return current, nil
}

func resolveSymlinkChain(path, canonicalRoot string, hops *int) (string, error) {
	current := path
	for {
		info, err := os.Lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return current, nil
			}
			return "", fmt.Errorf("safepath: inspect %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return current, nil
		}
		*hops++
		if *hops > maxSymlinkHops {
			return "", fmt.Errorf("safepath: %s: too many levels of symbolic links", path)
		}
		target, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("safepath: read symlink %s: %w", current, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		target = filepath.Clean(target)
		if !isWithin(canonicalRoot, target) {
			return "", fmt.Errorf("safepath: %s escapes %s through a symlink", current, canonicalRoot)
		}
		current = target
	}
}

func isWithin(root, target string) bool {
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
}

// ValidateConfiguredRoot checks an administrator-supplied absolute storage
// path (for example JAR_STORAGE_PATH or a backup archive path) for basic
// sanity. Unlike server-relative paths validated by Contain, a configured
// root is expected to live outside any single server's directory, so this
// only rejects an empty value, a relative path, an unclean path, and the
// filesystem root itself.
func ValidateConfiguredRoot(path string) error {
	if path == "" {
		return errors.New("safepath: configured path must not be empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("safepath: %q must be an absolute path", path)
	}
	if cleaned := filepath.Clean(path); cleaned != path {
		return fmt.Errorf("safepath: %q is not a clean path (expected %q)", path, cleaned)
	}
	if path == string(filepath.Separator) {
		return errors.New("safepath: refusing to use the filesystem root as a configured path")
	}
	return nil
}

// CaseInsensitiveCollision reports an existing entry in dir whose name
// matches name case-insensitively but not exactly. Server, JAR group and
// world names must stay unique regardless of the underlying filesystem's
// case sensitivity, since an installation can move between a case-sensitive
// and a case-preserving-only volume. A missing dir is not a collision.
func CaseInsensitiveCollision(dir, name string) (existing string, found bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("safepath: list %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.Name() == name {
			continue
		}
		if strings.EqualFold(entry.Name(), name) {
			return entry.Name(), true, nil
		}
	}
	return "", false, nil
}
