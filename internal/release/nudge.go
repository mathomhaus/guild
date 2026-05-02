package release

import "fmt"

// BuildMessage returns a single-line nudge string for display to the user.
//
// For minor/patch upgrades: "up guild <latest> available (you have <current>). See <url>"
// For major upgrades: the message includes a breaking-changes warning.
//
// ASCII-only except for the up-arrow prefix (U+2191, widely supported).
// No em dashes (U+2014).
func BuildMessage(current, latest, url string, majorGap bool) string {
	if majorGap {
		return fmt.Sprintf(
			"^ guild %s (major release) available; breaking changes possible. See %s",
			latest, url,
		)
	}
	return fmt.Sprintf(
		"^ guild %s available (you have %s). See %s",
		latest, current, url,
	)
}
