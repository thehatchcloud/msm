package screen

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// Version is a screen release, such as 4.09.01 (4, 9, 1).
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%02d.%02d", v.Major, v.Minor, v.Patch) }

// MinimumVersion is the oldest screen this adapter accepts. Every
// capability it uses (-ls, -dmS with an argument vector, -S <pid.name>
// -p 0 -X stuff, -r, SCREENDIR) predates 4.0, but only the versions listed
// in docs/development.md are tested.
var MinimumVersion = Version{4, 0, 0}

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
		return v, fmt.Errorf("screen: version %s is older than the supported minimum %s", v, MinimumVersion)
	}
	return v, nil
}
