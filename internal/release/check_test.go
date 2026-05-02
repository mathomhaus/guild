package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockGitHubServer returns a test server that responds with the given
// status code and JSON body (if statusCode == 200).
func mockGitHubServer(t *testing.T, statusCode int, tag, htmlURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if statusCode == http.StatusOK {
			body, _ := json.Marshal(map[string]string{
				"tag_name": tag,
				"html_url": htmlURL,
			})
			_, _ = w.Write(body)
		}
	}))
}

// withCacheDir sets a temp dir as the home for this test so cache reads/writes
// use an isolated location. Returns a cleanup function.
func withCacheDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	// Pre-create .guild dir.
	if err := os.MkdirAll(filepath.Join(tmp, ".guild"), 0o700); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.UserHomeDir()
	t.Setenv("HOME", tmp)
	t.Cleanup(func() { t.Setenv("HOME", orig) })
	return tmp
}

// overrideLatestURL temporarily replaces the API URL used by LatestRelease
// with the given test server URL. We patch it by swapping a package-level var.
// Since check.go uses the constant, we accept the package-level var approach:
// for test isolation we duplicate the fetch inline via a helper.

// TestIsNewer covers the semver comparison logic.
func TestIsNewer(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		// Standard semver.
		{"v0.2.1", "v0.3.0", true},
		{"v0.3.0", "v0.3.0", false},
		{"v0.3.1", "v0.3.0", false}, // local is ahead
		// Git describe suffix stripped correctly.
		{"v0.2.1-44-g2e6aba5", "v0.3.0", true},
		{"v0.3.0-1-gabcdef0", "v0.3.0", false},
		// Non-semver current: silent skip.
		{"dev", "v0.3.0", false},
		{"", "v0.3.0", false},
		// Invalid latest: silent skip.
		{"v0.2.1", "not-semver", false},
		// Major gap.
		{"v0.2.1", "v1.0.0", true},
	}

	for _, tc := range cases {
		got, err := IsNewer(tc.current, tc.latest)
		if err != nil {
			t.Errorf("IsNewer(%q, %q): unexpected error: %v", tc.current, tc.latest, err)
			continue
		}
		if got != tc.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

// TestBuildMessage verifies nudge string format.
func TestBuildMessage(t *testing.T) {
	minor := BuildMessage("v0.2.1", "v0.3.0", "https://github.com/mathomhaus/guild/releases/tag/v0.3.0", false)
	major := BuildMessage("v0.2.1", "v1.0.0", "https://github.com/mathomhaus/guild/releases/tag/v1.0.0", true)

	if minor == "" {
		t.Error("minor nudge: expected non-empty string")
	}
	if major == "" {
		t.Error("major nudge: expected non-empty string")
	}
	// Must not contain em dash.
	for _, s := range []string{minor, major} {
		for _, r := range s {
			if r == '—' {
				t.Errorf("nudge string contains em dash: %q", s)
			}
		}
	}
	// Minor nudge should mention current version.
	if minor != "^ guild v0.3.0 available (you have v0.2.1). See https://github.com/mathomhaus/guild/releases/tag/v0.3.0" {
		t.Errorf("minor nudge unexpected format: %q", minor)
	}
	// Major nudge should mention breaking changes.
	if major != "^ guild v1.0.0 (major release) available; breaking changes possible. See https://github.com/mathomhaus/guild/releases/tag/v1.0.0" {
		t.Errorf("major nudge unexpected format: %q", major)
	}
}

// TestCache verifies round-trip cache read/write and the freshness check.
func TestCache(t *testing.T) {
	withCacheDir(t)

	entry := CacheEntry{
		CheckedAt: time.Now().UTC(),
		Latest:    "v0.3.0",
		URL:       "https://example.com",
	}
	if err := WriteCache(entry); err != nil {
		t.Fatalf("WriteCache: %v", err)
	}

	got, err := ReadCache()
	if err != nil {
		t.Fatalf("ReadCache: %v", err)
	}
	if got.Latest != entry.Latest {
		t.Errorf("got.Latest = %q, want %q", got.Latest, entry.Latest)
	}
	if got.URL != entry.URL {
		t.Errorf("got.URL = %q, want %q", got.URL, entry.URL)
	}

	// Fresh cache should satisfy 24h window.
	if time.Since(got.CheckedAt) >= 24*time.Hour {
		t.Error("fresh cache should be within 24h")
	}
}

// TestCacheMalformed verifies that malformed JSON returns an error (treated as stale).
func TestCacheMalformed(t *testing.T) {
	home := withCacheDir(t)
	path := filepath.Join(home, ".guild", cacheFileName)
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadCache()
	if err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}

// TestCacheMissing verifies that a missing cache file returns an error.
func TestCacheMissing(t *testing.T) {
	withCacheDir(t)
	// Do not write any cache file.
	_, err := ReadCache()
	if err == nil {
		t.Error("expected error for missing cache file, got nil")
	}
}

// TestLatestRelease_200 verifies successful HTTP parse via httptest.
func TestLatestRelease_200(t *testing.T) {
	srv := mockGitHubServer(t, http.StatusOK, "v0.3.0", "https://example.com/rel")
	defer srv.Close()

	// Temporarily swap the URL used by LatestRelease.
	orig := latestReleaseURL
	setLatestReleaseURL(srv.URL)
	defer setLatestReleaseURL(orig)

	rel, err := LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Tag != "v0.3.0" {
		t.Errorf("Tag = %q, want %q", rel.Tag, "v0.3.0")
	}
}

// TestLatestRelease_404 verifies ErrNoReleases on 404.
func TestLatestRelease_404(t *testing.T) {
	srv := mockGitHubServer(t, http.StatusNotFound, "", "")
	defer srv.Close()

	orig := latestReleaseURL
	setLatestReleaseURL(srv.URL)
	defer setLatestReleaseURL(orig)

	_, err := LatestRelease(context.Background())
	if err == nil || err != ErrNoReleases {
		t.Errorf("expected ErrNoReleases, got %v", err)
	}
}

// TestLatestRelease_Timeout verifies silent handling on network timeout.
func TestLatestRelease_Timeout(t *testing.T) {
	// Use a server that never responds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until client times out.
		<-r.Context().Done()
	}))
	defer srv.Close()

	orig := latestReleaseURL
	setLatestReleaseURL(srv.URL)
	defer setLatestReleaseURL(orig)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := LatestRelease(ctx)
	if err == nil {
		t.Error("expected timeout error, got nil")
	}
	// Caller (CheckAndNudge) will swallow this; just verify err is non-nil.
}

// TestLatestRelease_MalformedJSON verifies silent handling on bad JSON body.
func TestLatestRelease_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{bad json"))
	}))
	defer srv.Close()

	orig := latestReleaseURL
	setLatestReleaseURL(srv.URL)
	defer setLatestReleaseURL(orig)

	_, err := LatestRelease(context.Background())
	if err == nil {
		t.Error("expected parse error, got nil")
	}
}

// TestCheckAndNudge_OptOut verifies GUILD_NO_UPDATE_CHECK=1 disables all checks.
func TestCheckAndNudge_OptOut(t *testing.T) {
	withCacheDir(t)
	t.Setenv("GUILD_NO_UPDATE_CHECK", "1")

	// Even if a cache exists with a newer version, no nudge should fire.
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    "v9.9.9",
		URL:       "https://example.com",
	})

	msg := CheckAndNudge(context.Background(), "v0.1.0", true)
	if msg != "" {
		t.Errorf("expected empty string with opt-out, got %q", msg)
	}
}

// TestCheckAndNudge_NonTTY verifies nudge is suppressed when isTTY=false.
func TestCheckAndNudge_NonTTY(t *testing.T) {
	withCacheDir(t)
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    "v9.9.9",
		URL:       "https://example.com",
	})

	msg := CheckAndNudge(context.Background(), "v0.1.0", false)
	if msg != "" {
		t.Errorf("expected empty string for non-TTY, got %q", msg)
	}
}

// TestCheckAndNudge_UpToDate verifies no nudge when already on latest.
func TestCheckAndNudge_UpToDate(t *testing.T) {
	withCacheDir(t)
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    "v0.3.0",
		URL:       "https://example.com",
	})

	msg := CheckAndNudge(context.Background(), "v0.3.0", true)
	if msg != "" {
		t.Errorf("expected empty string when up-to-date, got %q", msg)
	}
}

// TestCheckAndNudge_Newer verifies nudge fires on newer version from cache.
func TestCheckAndNudge_Newer(t *testing.T) {
	withCacheDir(t)
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    "v0.5.0",
		URL:       "https://example.com/v0.5.0",
	})

	msg := CheckAndNudge(context.Background(), "v0.3.0", true)
	if msg == "" {
		t.Error("expected nudge string, got empty")
	}
}

// TestCheckAndNudge_CacheWithin24h verifies stale cache triggers HTTP,
// and fresh cache skips HTTP (by using a server that panics if called).
func TestCheckAndNudge_CacheFreshSkipsHTTP(t *testing.T) {
	withCacheDir(t)

	// Write a fresh cache entry (just now).
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    "v0.4.0",
		URL:       "https://example.com/v0.4.0",
	})

	// Point latestReleaseURL at a server that fails if called.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP should not be called when cache is fresh")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	orig := latestReleaseURL
	setLatestReleaseURL(srv.URL)
	defer setLatestReleaseURL(orig)

	msg := CheckAndNudge(context.Background(), "v0.3.0", true)
	if msg == "" {
		t.Error("expected nudge from cache, got empty")
	}
}

// TestCheckAndNudge_StaleTriggersHTTP verifies a stale cache causes a live HTTP call.
func TestCheckAndNudge_StaleTriggersHTTP(t *testing.T) {
	withCacheDir(t)

	// Write a 25-hour-old cache entry (stale).
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now().Add(-25 * time.Hour),
		Latest:    "v0.3.0",
		URL:       "https://example.com/old",
	})

	// Serve a newer version from the mock server.
	srv := mockGitHubServer(t, http.StatusOK, "v0.5.0", "https://example.com/v0.5.0")
	defer srv.Close()

	orig := latestReleaseURL
	setLatestReleaseURL(srv.URL)
	defer setLatestReleaseURL(orig)

	msg := CheckAndNudge(context.Background(), "v0.3.0", true)
	if msg == "" {
		t.Error("expected nudge after stale cache triggers HTTP, got empty")
	}
}

// TestCheckAndNudge_DevVersion verifies that "dev" version silently skips nudge.
func TestCheckAndNudge_DevVersion(t *testing.T) {
	withCacheDir(t)
	_ = WriteCache(CacheEntry{
		CheckedAt: time.Now(),
		Latest:    "v0.5.0",
		URL:       "https://example.com/v0.5.0",
	})

	msg := CheckAndNudge(context.Background(), "dev", true)
	if msg != "" {
		t.Errorf("expected empty string for dev version, got %q", msg)
	}
}
