package release

import (
	"regexp"

	"golang.org/x/mod/semver"
)

// devSuffixRE matches the "-N-g<sha>" suffix appended by `git describe`
// to a base tag. For example "v0.2.1-44-g2e6aba5" -> stripped to "v0.2.1".
var devSuffixRE = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)

// stripDevSuffix removes the `git describe` dev build suffix from s,
// returning the canonical semver base tag.
func stripDevSuffix(s string) string {
	return devSuffixRE.ReplaceAllString(s, "")
}

// IsNewer reports whether latest is strictly newer than current.
//
// Rules:
//   - If current does not parse as semver (after stripping the dev suffix),
//     return (false, nil) to skip the nudge silently.
//   - If current is >= latest (same version or local build is ahead),
//     return (false, nil) so we emit nothing.
//   - Returns (true, nil) only when latest is strictly newer.
func IsNewer(current, latest string) (bool, error) {
	c := stripDevSuffix(current)
	if !semver.IsValid(c) {
		return false, nil
	}
	if !semver.IsValid(latest) {
		return false, nil
	}
	return semver.Compare(latest, c) > 0, nil
}

// isMajorGap reports whether latest is in a higher major version than current.
// Returns (isMajor=false, ok=false) on any parse failure so callers degrade to minor nudge.
func isMajorGap(current, latest string) (isMajor, ok bool) {
	c := stripDevSuffix(current)
	cm := semver.Major(c)
	lm := semver.Major(latest)
	if cm == "" || lm == "" {
		return false, false
	}
	return cm != lm, true
}
