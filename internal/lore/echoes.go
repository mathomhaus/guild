package lore

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Echo pairs a stale entry with the reason we decided it was stale.
// The CLI prints both so operators can reforge with context.
type Echo struct {
	Entry  *Entry
	Reason string
}

// Echoes returns entries whose valid_days threshold has passed, plus
// optionally (gitAware=true) entries whose file_path has been modified
// in git since the entry was written, plus entries carrying a persisted
// staleness signal (see staleness.go). Only current-status entries are
// considered (archived/superseded are already out of circulation), and
// that same filter is what retires persisted signals: a signal row only
// surfaces while its entry is still in this scan.
//
// The git-aware branch shells out to `git log` per entry; failures
// degrade silently so missing git / non-repo paths don't block the
// time-based signal.
//
// Union semantics: one echo line per entry. When an entry is flagged by
// both a query-time check and a persisted signal, the signal reason is
// suffixed to the query-time reason ("<query reason>; <signal reason>").
// An entry flagged only by a persisted signal surfaces with the
// persisted reason even when gitAware=false. With zero signal rows the
// output is identical to the signal-unaware behavior.
func Echoes(ctx context.Context, db *sql.DB, project string, gitAware bool) ([]Echo, error) {
	if db == nil {
		return nil, fmt.Errorf("lore: echoes: nil db")
	}
	if strings.TrimSpace(project) == "" {
		return nil, fmt.Errorf("lore: echoes: project required")
	}

	signals, err := loadStalenessSignals(ctx, db, project)
	if err != nil {
		return nil, fmt.Errorf("lore: echoes: %w", err)
	}

	//nolint:gosec // G202: entryColumns is a constant; no user input reaches the SQL text
	sqlText := `SELECT ` + entryColumns + `
		FROM entries e
		WHERE e.project_id = ? AND e.status = 'current'
		ORDER BY e.created_at ASC`
	rows, err := db.QueryContext(ctx, sqlText, project) //sqlcheck:ignore // sqlText is a constant template; entryColumns is a constant
	if err != nil {
		return nil, fmt.Errorf("lore: echoes: query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	now := time.Now().UTC()
	// Per-batch repo-root cache. Without it defaultGitFileLastModified
	// shells out to `git rev-parse --show-toplevel` once per entry, so a
	// list of N entries in the same repo costs 2N subprocess spawns
	// instead of N+1. The resolver lives only for this Echoes call so
	// state cannot leak between batches or across repos.
	if gitAware {
		ctx = withRepoRootResolver(ctx, newRepoRootResolver())
	}
	var stale []Echo
	for rows.Next() {
		e := &Entry{}
		if err := scanEntry(rows, e); err != nil {
			return nil, err
		}
		reason := echoReason(ctx, e, now, gitAware)
		if sig, ok := signals[e.ID]; ok {
			if reason == "" {
				reason = sig
			} else {
				reason = reason + "; " + sig
			}
		}
		if reason != "" {
			stale = append(stale, Echo{Entry: e, Reason: reason})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lore: echoes: iterate: %w", err)
	}
	return stale, nil
}

// echoReason returns a non-empty reason string iff the entry should be
// flagged as a fading echo under the provided policy.
func echoReason(ctx context.Context, e *Entry, now time.Time, gitAware bool) string {
	age := int(daysBetween(e.CreatedAt, now))
	if e.ValidDays != nil && *e.ValidDays > 0 && age >= *e.ValidDays {
		return fmt.Sprintf("%dd old (valid: %dd)", age, *e.ValidDays)
	}
	if gitAware && e.FilePath != "" {
		if gitDate := gitFileLastModified(ctx, e.FilePath); !gitDate.IsZero() && gitDate.After(e.CreatedAt) {
			return "file modified after entry was created"
		}
	}
	return ""
}

// gitFileLastModifiedFn is the seam for the git-aware staleness check.
// Tests replace it to exercise the logic without a real git repo or a
// specific process CWD.
var gitFileLastModifiedFn = defaultGitFileLastModified

// gitFileLastModified is the internal call site; it delegates to the
// injectable seam so tests can substitute behaviour.
func gitFileLastModified(ctx context.Context, path string) time.Time {
	return gitFileLastModifiedFn(ctx, path)
}

// defaultGitFileLastModified shells out to `git log -1 --format=%aI -- <path>`
// and returns the last-modified timestamp parsed to a time.Time.
// Returns the zero time when git is missing, the file is not tracked,
// or any error occurs — best-effort.
//
// The command is run with its working directory set to the directory that
// contains the file, not the process CWD. This ensures the correct
// repository context is used when guild is launched from outside the
// target project's directory tree (e.g. MCP sessions, project overrides).
func defaultGitFileLastModified(ctx context.Context, path string) time.Time {
	// Bound the command with a 5-second timeout so a stuck git
	// doesn't hang the lore read.
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Derive the repository root from the file's own directory so the
	// query is never sensitive to the guild process's ambient CWD.
	// Use the per-batch resolver if Echoes installed one, otherwise
	// fall back to a one-shot subprocess (preserves single-call usage).
	dir := filepath.Dir(path)
	repoRoot := resolveRepoRoot(cmdCtx, dir)
	if repoRoot == "" {
		// git missing, not a repo, or dir doesn't exist — degrade quietly.
		return time.Time{}
	}

	//nolint:gosec // arguments are hard-coded + file path from DB
	cmd := exec.CommandContext(cmdCtx, "git", "log", "-1", "--format=%aI", "--", path)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return time.Time{}
	}
	return parseSQLiteTime(s)
}

// repoRootResolver memoizes `git rev-parse --show-toplevel` results
// across calls within a single Echoes() pass. Without it a list of N
// entries in the same repo costs 2N subprocess spawns instead of N+1.
//
// The lookup function is injected so tests can verify the cache
// behaviour without invoking real git or a real filesystem.
type repoRootResolver struct {
	mu     sync.Mutex
	cache  map[string]string // dir -> repo root, "" when dir is not in a repo
	lookup func(ctx context.Context, dir string) string
}

func newRepoRootResolver() *repoRootResolver {
	return &repoRootResolver{
		cache:  map[string]string{},
		lookup: defaultRepoRootLookup,
	}
}

// Resolve returns the git repo root for dir, using the injected
// lookup the first time and a cached value on subsequent calls.
// An empty string is itself a cache entry, so a non-repo directory
// only spawns one subprocess across the batch.
func (r *repoRootResolver) Resolve(ctx context.Context, dir string) string {
	r.mu.Lock()
	if cached, ok := r.cache[dir]; ok {
		r.mu.Unlock()
		return cached
	}
	r.mu.Unlock()

	root := r.lookup(ctx, dir)

	r.mu.Lock()
	r.cache[dir] = root
	r.mu.Unlock()
	return root
}

// defaultRepoRootLookup is the production implementation: shell out to
// `git rev-parse --show-toplevel` with the directory as the cwd.
// Returns "" on any error so the caller can degrade quietly.
func defaultRepoRootLookup(ctx context.Context, dir string) string {
	//nolint:gosec // arguments are hard-coded + dir is derived from a DB-stored file path
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type repoRootResolverKey struct{}

// withRepoRootResolver attaches a per-batch resolver to ctx. Echoes
// installs one before iterating its row cursor; resolveRepoRoot picks
// it up from the same ctx (or any derived ctx, including the timeout
// ctx defaultGitFileLastModified creates).
func withRepoRootResolver(ctx context.Context, r *repoRootResolver) context.Context {
	return context.WithValue(ctx, repoRootResolverKey{}, r)
}

// resolveRepoRoot returns the git repo root for dir using the cached
// resolver attached to ctx, or a one-shot lookup if no resolver is
// installed. The fall-back keeps callers other than Echoes (and the
// existing direct unit tests) working without changes.
func resolveRepoRoot(ctx context.Context, dir string) string {
	if r, ok := ctx.Value(repoRootResolverKey{}).(*repoRootResolver); ok && r != nil {
		return r.Resolve(ctx, dir)
	}
	return defaultRepoRootLookup(ctx, dir)
}
