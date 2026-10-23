package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/thehatchcloud/msm/internal/process"
)

// rdiff-backup is a separate program (Python, with librsync), not something
// this binary embeds. The adapter runs it directly, with an argument vector
// and never through a shell, the way the legacy manager did:
//
//	rdiff-backup <world> <WORLD_RDIFF_PATH>/<server>/<world>
//	rdiff-backup --remove-older-than <ROTATION>D --force <repository>
//
// under nice -n <RDIFF_BACKUP_NICE>. rdiff-backup 2.2 deprecated that command
// line for one with actions (backup, list, remove), and 3.x removes it, so
// the adapter asks the installed version which to use. The repository format
// is rdiff-backup's own and is not changed: an existing repository from the
// legacy manager is read and extended as before.
var (
	// ErrToolMissing is an enabled method whose program is not installed.
	ErrToolMissing = errors.New("backup: a program an enabled backup method needs is not installed")
	// ErrToolVersion is an installed program this adapter cannot drive.
	ErrToolVersion = errors.New("backup: the installed rdiff-backup is not usable")
	// ErrRepository is an rdiff-backup repository that cannot be used.
	ErrRepository = errors.New("backup: the rdiff-backup repository cannot be used")
)

const rdiffData = "rdiff-backup-data"

// rdiffTool is an installed rdiff-backup.
type rdiffTool struct {
	path    string
	nice    string // path of nice, or "" to run at the current priority
	version string
	// actions is whether this version takes backup/list/remove actions.
	actions bool
	niceVal int
}

var versionRe = regexp.MustCompile(`(?m)^rdiff-backup\s+(\d+)\.(\d+)(?:\.(\d+))?`)

// findRdiff locates rdiff-backup and checks that its version can be driven.
func (m *Manager) findRdiff(ctx context.Context, nice int) (*rdiffTool, error) {
	path, err := m.cfg.LookPath("rdiff-backup")
	if err != nil {
		return nil, fmt.Errorf("%w: RDIFF_BACKUP_ENABLED is true, but rdiff-backup was not found in PATH (%v); install it (for example `apt install rdiff-backup` or `brew install rdiff-backup`) or set RDIFF_BACKUP_ENABLED=false; ZIP and hard-link snapshot backups do not need it", ErrToolMissing, err)
	}
	res, err := m.cfg.Runner.Run(ctx, process.Command{Path: path, Args: []string{"--version"}})
	if err != nil {
		return nil, fmt.Errorf("%w: %s --version failed: %v%s", ErrToolVersion, path, err, tail(res.Stderr))
	}
	match := versionRe.FindStringSubmatch(res.Stdout)
	if match == nil {
		return nil, fmt.Errorf("%w: %s --version printed %q, which is not a version", ErrToolVersion, path, strings.TrimSpace(res.Stdout))
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	if major < 1 {
		return nil, fmt.Errorf("%w: version %s.%s is too old; the manager supports rdiff-backup 1.x and later", ErrToolVersion, match[1], match[2])
	}
	t := &rdiffTool{path: path, version: strings.TrimSpace(match[0][len("rdiff-backup"):]), niceVal: nice,
		actions: major > 2 || (major == 2 && minor >= 2)}
	if nice != 0 {
		if t.nice, err = m.cfg.LookPath("nice"); err != nil {
			return nil, fmt.Errorf("%w: RDIFF_BACKUP_NICE is %d, which needs the `nice` program, and it was not found in PATH (%v); set RDIFF_BACKUP_NICE=0 to run rdiff-backup at the normal priority", ErrToolMissing, nice, err)
		}
	}
	return t, nil
}

// command is the invocation of rdiff-backup with args, under nice.
func (t *rdiffTool) command(dir string, args ...string) process.Command {
	if t.nice == "" {
		return process.Command{Path: t.path, Args: args, Dir: dir}
	}
	return process.Command{Path: t.nice, Args: append([]string{"-n", strconv.Itoa(t.niceVal), t.path}, args...), Dir: dir}
}

func (t *rdiffTool) backupArgs(src, repo string) []string {
	if t.actions {
		return []string{"backup", src, repo}
	}
	return []string{src, repo}
}

func (t *rdiffTool) removeArgs(days int, repo string) []string {
	age := strconv.Itoa(days) + "D"
	if t.actions {
		return []string{"--force", "remove", "increments", "--older-than", age, repo}
	}
	return []string{"--remove-older-than", age, "--force", repo}
}

func (t *rdiffTool) listArgs(repo string) []string {
	if t.actions {
		return []string{"list", "increments", repo}
	}
	return []string{"--list-increments", repo}
}

// nothingToRemove is rdiff-backup's way of saying that no increment is old
// enough to remove yet: it exits with status 2, "finished with warnings",
// and says so. That is the normal state of a young repository, not a
// failure; any other status 2 is.
func nothingToRemove(err error, stderr string) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 2 && strings.Contains(stderr, "No increment is older than")
}

// tail is the last lines of a program's error output, ready to append to a
// message.
func tail(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return ": " + strings.Join(lines, " | ")
}

// checkRepository refuses a destination that is neither empty nor an
// rdiff-backup repository the installed version can read, before the server
// is touched. A repository an interrupted backup left needs no action here:
// rdiff-backup regresses it itself on the next run.
func (m *Manager) checkRepository(ctx context.Context, t *rdiffTool, repo string) error {
	entries, err := os.ReadDir(repo)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRepository, err)
	}
	if len(entries) == 0 {
		return nil
	}
	if info, err := os.Lstat(filepath.Join(repo, rdiffData)); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s is not empty and is not an rdiff-backup repository (it has no %s directory); rdiff-backup would refuse it, so no backup was started", ErrRepository, repo, rdiffData)
	}
	res, err := m.cfg.Runner.Run(ctx, t.command(repo, t.listArgs(repo)...))
	if err != nil {
		return fmt.Errorf("%w: the existing repository %s cannot be read by rdiff-backup %s: %v%s", ErrRepository, repo, t.version, err, tail(res.Stderr))
	}
	return nil
}

// rdiffBackup adds src to the repository and then removes increments older
// than the retention.
func (m *Manager) rdiffBackup(ctx context.Context, t *rdiffTool, src, repo string, days int) error {
	res, err := m.cfg.Runner.Run(ctx, t.command(repo, t.backupArgs(src, repo)...))
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("rdiff-backup of %s did not finish: %w", src, cause)
		}
		return fmt.Errorf("rdiff-backup of %s into %s failed: %v%s", src, repo, err, tail(res.Stderr))
	}
	res, err = m.cfg.Runner.Run(ctx, t.command(repo, t.removeArgs(days, repo)...))
	if err != nil && !nothingToRemove(err, res.Stderr) {
		if cause := context.Cause(ctx); cause != nil {
			return fmt.Errorf("the rdiff-backup increment was made in %s but removing old ones did not finish: %w", repo, cause)
		}
		return fmt.Errorf("the rdiff-backup increment was made in %s, but removing increments older than %d days failed: %v%s", repo, days, err, tail(res.Stderr))
	}
	return nil
}
