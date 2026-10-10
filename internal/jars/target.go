package jars

import (
	"fmt"
	"net/url"
	"strings"
)

// TargetKind is what a group's target names.
type TargetKind int

const (
	// TargetURL downloads one file from an https URL.
	TargetURL TargetKind = iota
	// TargetRelease and TargetSnapshot download Mojang's newest release or
	// snapshot server through the version manifest, verifying its SHA-1.
	TargetRelease
	TargetSnapshot
)

// Target is a parsed download target.
type Target struct {
	Kind TargetKind
	Raw  string
	URL  *url.URL // TargetURL only
}

// ParseTarget accepts what jargroup_getlatest accepted, less what cannot be
// downloaded safely:
//
//   - "minecraft-snapshot", Mojang's newest snapshot;
//   - any other word starting with "minecraft" (normally "minecraft"),
//     Mojang's newest release;
//   - an https URL. A plain http URL is refused, since what it serves could
//     not be authenticated (DEV-020).
func ParseTarget(s string) (Target, error) {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return Target{}, fmt.Errorf("%w %q: it must be one word, an https URL or minecraft", ErrInvalidTarget, s)
	}
	switch {
	case s == "minecraft-snapshot":
		return Target{Kind: TargetSnapshot, Raw: s}, nil
	case strings.HasPrefix(s, "minecraft") && !strings.Contains(s, ":"):
		return Target{Kind: TargetRelease, Raw: s}, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return Target{}, fmt.Errorf("%w %q: not an https URL, minecraft or minecraft-snapshot", ErrInvalidTarget, s)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return Target{Kind: TargetURL, Raw: s, URL: u}, nil
	case "http":
		return Target{}, fmt.Errorf("%w: %q; use an https URL, since a file fetched over http could have been altered on the way", ErrInsecureURL, s)
	default:
		return Target{}, fmt.Errorf("%w %q: only https URLs are supported", ErrInvalidTarget, s)
	}
}
