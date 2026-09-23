package screen

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Version is a screen release, such as 4.09.01 (4, 9, 1).
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%02d.%02d", v.Major, v.Minor, v.Patch) }

// MinimumVersion is the oldest screen this adapter accepts. The 4.00.03
// that macOS bundles as /usr/bin/screen fails the native tests (its window
// process is never observed as a child of the session's screen process),
// so 4.00.x is refused with advice to install a current screen. Only the
// versions listed in docs/development.md are tested.
var MinimumVersion = Version{4, 1, 0}

// ErrUnsupportedVersion reports a screen older than MinimumVersion.
var ErrUnsupportedVersion = errors.New("screen: unsupported screen version")

var versionPattern = regexp.MustCompile(`Screen version (\d+)\.(\d+)(?:\.(\d+))?`)

func parseVersion(out string) (Version, error) {
	m := versionPattern.FindStringSubmatch(out)
	if m == nil {
		return Version{}, fmt.Errorf("screen: unrecognized version output %q", out)
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3]) // absent patch parses as 0
	return v, nil
}

func (v Version) less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// Version runs `screen -v` and rejects a release older than
// MinimumVersion. Screen's exit status for -v differs between releases,
// so only the output is trusted.
func (b *Backend) Version(ctx context.Context) (Version, error) {
	result, err := b.cfg.Runner.Run(ctx, b.command("/", nil, "-v"))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Version{}, ctxErr
	}
	v, parseErr := parseVersion(result.Stdout + result.Stderr)
	if parseErr != nil {
		if err != nil {
			return Version{}, fmt.Errorf("screen: run %s -v: %w", b.cfg.Screen, err)
		}
		return Version{}, parseErr
	}
	if v.less(MinimumVersion) {
		return v, fmt.Errorf("%w: %s at %s is older than %s; install a current GNU screen (for example `brew install screen` on macOS) and configure its path",
			ErrUnsupportedVersion, v, b.cfg.Screen, MinimumVersion)
	}
	return v, nil
}
