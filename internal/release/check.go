// Package release provides upgrade-availability checks for the guild binary.
//
// Design rules:
//   - Silent on ANY failure: network errors, DNS misses, 5xx, malformed JSON,
//     cache IO errors. The nudge is a nice-to-have; never a noise generator.
//   - Zero network on warm cache: the 24h window is a hard promise.
//   - Opt-out: GUILD_NO_UPDATE_CHECK=1 disables entirely.
//   - No authenticated GitHub API requests.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// ErrNoReleases is returned by LatestRelease when the GitHub API responds
// with 404, meaning no full (non-pre) releases have been published yet.
var ErrNoReleases = errors.New("no releases published")

// Release holds the fields parsed from the GitHub releases/latest response.
type Release struct {
	// Tag is the semver tag string, e.g. "v0.3.0".
	Tag string
	// URL is the HTML URL to the release page.
	URL string
}

// githubRelease is the JSON shape for the GitHub releases/latest endpoint.
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// latestReleaseURL is the canonical GitHub API endpoint for the upstream repo.
// Unauthenticated; rate limit is 60 req/hour per IP, well above our 24h cache window.
// Package-level var (not const) so tests can inject a mock server URL via setLatestReleaseURL.
var latestReleaseURL = "https://api.github.com/repos/mathomhaus/guild/releases/latest"

// setLatestReleaseURL replaces the URL used by LatestRelease. Intended for
// tests only; not safe for concurrent use outside test goroutines.
func setLatestReleaseURL(u string) { latestReleaseURL = u }

// defaultTimeout is the network timeout applied to LatestRelease when the
// caller's context does not already carry a tighter deadline.
const defaultTimeout = 2 * time.Second

// LatestRelease fetches the latest release tag and URL from the GitHub API.
//
// The passed ctx is honoured for cancellation; a 2s sub-deadline is applied
// internally so a slow GitHub response never blocks the caller beyond that.
//
// Returns ErrNoReleases on HTTP 404 (no full releases published).
// Returns an error on any other non-200 status or network/parse failure.
func LatestRelease(ctx context.Context) (Release, error) {
	// Apply a 2s sub-deadline unless the caller's context is already tighter.
	deadline := time.Now().Add(defaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, http.NoBody)
	if err != nil {
		return Release{}, fmt.Errorf("release: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("release: http get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return Release{}, ErrNoReleases
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("release: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return Release{}, fmt.Errorf("release: read body: %w", err)
	}

	var gr githubRelease
	if err := json.Unmarshal(body, &gr); err != nil {
		return Release{}, fmt.Errorf("release: parse json: %w", err)
	}
	if gr.TagName == "" {
		return Release{}, fmt.Errorf("release: empty tag_name in response")
	}

	return Release{Tag: gr.TagName, URL: gr.HTMLURL}, nil
}

// CheckAndNudge is the high-level adapter called by the CLI surface.
//
// It returns a non-empty nudge string when a newer release is available,
// and an empty string in all other cases (up-to-date, network failure,
// opt-out, non-TTY, etc.).
//
// current is the compiled-in version string (e.g. "v0.2.1" or
// "v0.2.1-44-g2e6aba5"). isTTY must be true; false suppresses the nudge
// for scripted/piped environments so stdout consumers are never polluted.
func CheckAndNudge(ctx context.Context, current string, isTTY bool) string {
	if os.Getenv("GUILD_NO_UPDATE_CHECK") == "1" {
		return ""
	}
	if !isTTY {
		return ""
	}
	return nudge(ctx, current)
}

// CheckAndNudgeMCP is the high-level adapter for the MCP guild_session_start
// surface. Unlike CheckAndNudge it omits the TTY gate (agents are never
// interactive terminals) but still respects GUILD_NO_UPDATE_CHECK and is
// silent on all failures.
func CheckAndNudgeMCP(ctx context.Context, current string) string {
	if os.Getenv("GUILD_NO_UPDATE_CHECK") == "1" {
		return ""
	}
	return nudge(ctx, current)
}

// nudge is the shared core: resolve latest, compare, build message.
// Returns "" on any failure or when not newer.
func nudge(ctx context.Context, current string) string {
	latest, _, err := latestWithCache(ctx)
	if err != nil {
		slog.Debug("release check failed", "err", err)
		return ""
	}
	if latest.Tag == "" {
		return ""
	}

	newer, err := IsNewer(current, latest.Tag)
	if err != nil || !newer {
		return ""
	}

	major, _ := isMajorGap(current, latest.Tag)
	return BuildMessage(current, latest.Tag, latest.URL, major)
}

// latestWithCache resolves the latest release, using the on-disk cache when
// it is fresh (within 24h). Returns the release, a bool indicating whether
// the cache was used, and an error.
func latestWithCache(ctx context.Context) (Release, bool, error) {
	cached, err := ReadCache()
	if err == nil && time.Since(cached.CheckedAt) < 24*time.Hour {
		return Release{Tag: cached.Latest, URL: cached.URL}, true, nil
	}

	rel, err := LatestRelease(ctx)
	if err != nil {
		if errors.Is(err, ErrNoReleases) {
			// Not an error worth surfacing; treat as "nothing to show".
			return Release{}, false, nil
		}
		return Release{}, false, err
	}

	// Best-effort cache write; failures are silently swallowed.
	if wErr := WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    rel.Tag,
		URL:       rel.URL,
	}); wErr != nil {
		slog.Debug("release cache write failed", "err", wErr)
	}

	return rel, false, nil
}
