package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mathomhaus/guild/internal/daemon"
	"github.com/mathomhaus/guild/internal/daemon/watch"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/mathomhaus/guild/internal/project"
	"github.com/mathomhaus/guild/internal/quest"
)

// This file is the host's half of the daemon watch -> staleness ->
// renewal pipeline (ADR-005 Phase 4). The pipeline in internal/daemon
// decides WHEN to act (a debounced file or git event); this seam supplies
// WHAT one event does: flag the citing lore entries stale (the staleness
// signals writer) and post capped, deduplicated renewal quests (the
// renewal poster). Keeping it here means internal/daemon never imports
// internal/lore, internal/quest, or internal/project, exactly the leaf
// discipline the idle scheduler's PassFunc already follows.
//
// Everything below is additive by construction: a file event writes
// signal rows and mints renewal quests, both new rows that touch nothing
// existing. The destructive judgment (re-validate vs. supersede vs.
// retire) is what each renewal quest routes to a human or interactive
// agent, so the pipeline runs unattended with the poster's journaling.

// watchRenewalAgent is the agent identity stamped on renewal quests the
// live watcher posts. Distinct from the idle pass's "sleep" agent so the
// board shows which path minted a renewal: event-driven (watch) vs. the
// scheduled dream pass (sleep).
const watchRenewalAgent = "watch"

// WatchRoots returns the daemon.RootsFunc that enumerates every registered
// project as a watch root. Each call opens a fresh lore.db handle (the
// per-call open discipline every tool uses; WAL makes it cheap) and maps
// project.List rows to watch.Root{Project: id, Path: absolute path}. The
// pipeline calls it at start and on every rescan, so a project registered
// after the daemon started is picked up without a restart.
func (h *DaemonHost) WatchRoots() daemon.RootsFunc {
	return func(ctx context.Context) ([]watch.Root, error) {
		db, err := openLoreDB(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp: watch roots: open lore db: %w", err)
		}
		defer func() { _ = db.Close() }()

		projects, err := project.List(ctx, db)
		if err != nil {
			return nil, fmt.Errorf("mcp: watch roots: list projects: %w", err)
		}
		roots := make([]watch.Root, 0, len(projects))
		for i := range projects {
			// projects.path is absolute (migration 001); the watcher
			// rejects relative paths, so a malformed row would fail the
			// whole rebuild. Skip blanks defensively instead.
			if projects[i].Path == "" {
				continue
			}
			roots = append(roots, watch.Root{
				Project: projects[i].ID,
				Path:    projects[i].Path,
			})
		}
		return roots, nil
	}
}

// WatchProcessor returns the daemon.ProcessFunc that turns one debounced
// watch event into staleness signals and renewal quests, capped at
// renewalCap quests per event. A non-positive cap records signals but
// posts nothing (the operator opted out of event-driven posting while
// keeping the flagging).
//
// File events flag every current entry citing the changed path via
// lore.FlagStaleByPath; git_head events run the project-scoped
// lore.GitSweep. Either way the flagged entry ids become renewal
// candidates (built from the project's fading echoes so each carries its
// human-readable reason), handed to quest.PostRenewals, which owns
// oldest-first selection, dedupe against open renewal quests, and the cap.
func (h *DaemonHost) WatchProcessor(renewalCap int) daemon.ProcessFunc {
	return func(ctx context.Context, ev watch.Event) (daemon.EventResult, error) {
		loreDB, err := openLoreDB(ctx)
		if err != nil {
			return daemon.EventResult{}, fmt.Errorf("mcp: watch process: open lore db: %w", err)
		}
		defer func() { _ = loreDB.Close() }()

		flagged, err := flagStaleForEvent(ctx, loreDB, ev)
		if err != nil {
			return daemon.EventResult{}, err
		}
		res := daemon.EventResult{Signals: len(flagged)}
		if len(flagged) == 0 || renewalCap <= 0 {
			return res, nil
		}

		candidates, err := renewalCandidatesFor(ctx, loreDB, ev.Project, flagged)
		if err != nil {
			return res, err
		}
		if len(candidates) == 0 {
			return res, nil
		}

		questDB, err := openQuestDB(ctx)
		if err != nil {
			return res, fmt.Errorf("mcp: watch process: open quest db: %w", err)
		}
		defer func() { _ = questDB.Close() }()

		post, err := quest.PostRenewals(ctx, questDB, ev.Project, candidates, renewalCap, watchRenewalAgent)
		if post != nil {
			res.QuestsPosted = len(post.Posted)
		}
		if err != nil {
			return res, fmt.Errorf("mcp: watch process: post renewals for %s: %w", ev.Project, err)
		}
		return res, nil
	}
}

// flagStaleForEvent persists staleness signals for one event and returns
// the flagged entry ids. A file event flags entries whose file_path
// matches the changed path; a git_head event sweeps the whole project for
// entries whose cited file moved in git after the entry was written.
func flagStaleForEvent(ctx context.Context, db *sql.DB, ev watch.Event) ([]int64, error) {
	switch ev.Kind {
	case watch.KindFile:
		ids, err := lore.FlagStaleByPath(ctx, db, ev.Project, ev.Path, lore.SourceWatcherFile, time.Time{})
		if err != nil {
			return nil, fmt.Errorf("mcp: watch process: flag stale by path %s: %w", ev.Path, err)
		}
		return ids, nil
	case watch.KindGitHead:
		ids, err := lore.GitSweep(ctx, db, ev.Project, time.Time{})
		if err != nil {
			return nil, fmt.Errorf("mcp: watch process: git sweep %s: %w", ev.Project, err)
		}
		return ids, nil
	default:
		// Unknown kind: nothing to flag. Forward-compatible with new event
		// kinds the watcher might add before this consumer learns them.
		return nil, nil
	}
}

// renewalCandidatesFor builds renewal candidates for the flagged entry ids
// in one project. It reads the project's fading echoes (which now include
// the just-written persisted signals) and keeps only the flagged ids, so
// each candidate carries the same human-readable reason the echo wall and
// the idle renewal step use. Entries that flagged but no longer surface as
// echoes (raced out of current status) are dropped.
func renewalCandidatesFor(ctx context.Context, db *sql.DB, projectID string, flagged []int64) ([]quest.RenewalCandidate, error) {
	want := make(map[int64]bool, len(flagged))
	for _, id := range flagged {
		want[id] = true
	}

	// gitAware=false: the persisted signal already surfaces the change, so
	// no per-entry git subprocess is needed on the event path. The echo's
	// reason then reads "file changed after entry was created" (the signal
	// reason), matching the watcher's intent.
	echoes, err := lore.Echoes(ctx, db, projectID, false)
	if err != nil {
		return nil, fmt.Errorf("mcp: watch process: echoes for %s: %w", projectID, err)
	}

	out := make([]quest.RenewalCandidate, 0, len(flagged))
	for i := range echoes {
		e := echoes[i].Entry
		if e == nil || !want[e.ID] {
			continue
		}
		out = append(out, quest.RenewalCandidate{
			EntryID:  e.ID,
			Title:    e.Title,
			Kind:     string(e.Kind),
			FilePath: e.FilePath,
			Reason:   echoes[i].Reason,
		})
	}
	return out, nil
}
