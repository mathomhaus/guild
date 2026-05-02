package mcp

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/quest"
)

// questEmbedProvider is the MCP-adapter lazy resolver for *quest.QuestEmbedDeps.
// It mirrors embedProvider for the lore side (QUEST-219 / LORE-371) but
// targets the quest corpus: QuestCorpus Index against quest.db, sharing the
// same Embedder and ModelID as the lore side.
//
// Lifecycle:
//   - Created once per server rebuild in Register(). Stored in
//     currentQuestEmbedProvider so buildMCPCommandDeps can wire it into
//     command.Deps.Embed.
//   - ResolveQuestEmbedDeps is called at the top of every quest_search handler
//     entry. On the common path (state unchanged since last resolve) it is one
//     indexed meta SELECT plus an RLock. On a state transition it upgrades to a
//     write lock, reconstructs *QuestEmbedDeps, caches, and logs once.
//
// State tracking: reads quest.embedder_state and quest.embedder_model_id from
// quest.db (the QuestCorpus MetaKey values). These are the same keys
// finalizeCorpusState writes during auto-backfill.
//
// Hexagonal: adapter concern only. internal/quest sees a *QuestEmbedDeps, not
// this type. The interface questEmbedResolver in quest/search_cmd.go is the
// declared port; questEmbedProvider satisfies it by name-matching.
type questEmbedProvider struct {
	mu          sync.RWMutex
	cached      *quest.QuestEmbedDeps
	lastState   string
	lastModelID string

	// loreProvider is the parent lore embedder resolver. When it returns a
	// non-nil *lore.EmbedDeps the quest provider borrows its Embedder and
	// ModelID to build the quest-side Index and QuestEmbedDeps. This avoids
	// re-probing or re-extracting the model binary; the embedder is loaded once.
	loreProvider *embedProvider

	// openDB opens a short-lived quest.db handle for meta reads and index load.
	openDB func(ctx context.Context) (*sql.DB, error)

	logger *slog.Logger
}

// newQuestEmbedProvider builds a provider with an unset cache.
func newQuestEmbedProvider(
	loreProvider *embedProvider,
	openDB func(ctx context.Context) (*sql.DB, error),
	logger *slog.Logger,
) *questEmbedProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &questEmbedProvider{
		loreProvider: loreProvider,
		openDB:       openDB,
		logger:       logger,
	}
}

// ResolveQuestEmbedDeps returns the current *quest.QuestEmbedDeps,
// reconstructing when meta.quest.embedder_state or quest.embedder_model_id
// change. A nil return is the BM25-only fallback path; quest_search tolerates it.
//
// This method satisfies the questEmbedResolver interface declared in
// internal/quest/search_cmd.go. Naming is intentional: the quest package
// type-switches d.Embed against the local interface shape, not an imported type.
func (p *questEmbedProvider) ResolveQuestEmbedDeps(ctx context.Context) *quest.QuestEmbedDeps {
	if p == nil {
		return nil
	}

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	state, modelID, err := p.readMetaState(readCtx)
	if err != nil {
		p.logger.Warn("quest embedder resolve: meta read failed; using cache",
			slog.String("err", err.Error()),
		)
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.cached
	}

	// Hot path: state unchanged since last resolve.
	p.mu.RLock()
	if p.lastState == state && p.lastModelID == modelID && p.lastState != "" {
		cached := p.cached
		p.mu.RUnlock()
		return cached
	}
	priorState := p.lastState
	p.mu.RUnlock()

	// Slow path: state changed or first resolve. Reconstruct outside the lock.
	reason := "initial_boot_enabled"
	if priorState != "" {
		reason = "state_flip_mid_session"
	}
	newDeps := p.reconstruct(ctx, reason)

	p.mu.Lock()
	p.cached = newDeps
	p.lastState = state
	p.lastModelID = modelID
	p.mu.Unlock()

	return newDeps
}

// reconstruct builds a *quest.QuestEmbedDeps by borrowing the embedder from
// the lore side and constructing a QuestCorpus Index against quest.db.
// Returns nil on any failure so callers fall back to BM25.
func (p *questEmbedProvider) reconstruct(ctx context.Context, reason string) *quest.QuestEmbedDeps {
	bootCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Borrow the lore-side embedder and model_id. The lore embedder is
	// already probed and extracted; no second extraction needed.
	loreDeps := p.loreProvider.ResolveEmbedDeps(bootCtx)
	if loreDeps == nil || !loreDeps.Enabled() {
		p.logger.Info("quest embedder wired lazily",
			slog.String("reason", reason),
			slog.String("status", "lore_embedder_not_ready"),
		)
		return nil
	}

	db, err := p.openDB(bootCtx)
	if err != nil {
		p.logger.Info("quest embedder wired lazily",
			slog.String("reason", reason),
			slog.String("status", "quest_db_open_failed"),
			slog.String("err", err.Error()),
		)
		return nil
	}
	defer func() { _ = db.Close() }()

	// Verify quest corpus embedder_state is "enabled" before loading the index.
	stateKey := embed.QuestCorpus{}.MetaKey(embed.FieldEmbedderState)
	state, stateErr := readMetaValue(bootCtx, db, stateKey)
	if stateErr != nil || state != "enabled" {
		reason2 := "meta_not_enabled"
		if stateErr != nil {
			reason2 = "meta_read_failed"
		}
		p.logger.Info("quest embedder wired lazily",
			slog.String("reason", reason),
			slog.String("status", reason2),
		)
		return nil
	}

	modelID := loreDeps.ModelID
	idx := embed.NewIndex(embed.QuestCorpus{}, modelID, embed.WithLogger(p.logger))
	n, lerr := idx.LoadFromDB(bootCtx, db)
	if lerr != nil {
		p.logger.Warn("quest embedder inactive: index load failed",
			slog.String("err", lerr.Error()),
			slog.String("model_id", modelID),
		)
		return nil
	}

	p.logger.Info("quest embedder wired lazily",
		slog.String("reason", reason),
		slog.String("status", "enabled"),
		slog.String("model_id", modelID),
		slog.Int("index_len", n),
	)

	return &quest.QuestEmbedDeps{
		Embedder: loreDeps.Embedder,
		Index:    idx,
		ModelID:  modelID,
	}
}

// readMetaState reads the quest.embedder_state and quest.embedder_model_id
// from quest.db so the hot-path compare can detect corpus state flips.
func (p *questEmbedProvider) readMetaState(ctx context.Context) (state, modelID string, err error) {
	db, oerr := p.openDB(ctx)
	if oerr != nil {
		return "", "", oerr
	}
	defer func() { _ = db.Close() }()

	corpus := embed.QuestCorpus{}
	state, err = readMetaValue(ctx, db, corpus.MetaKey(embed.FieldEmbedderState))
	if err != nil {
		return "", "", err
	}
	modelID, err = readMetaValue(ctx, db, corpus.MetaKey(embed.FieldEmbedderModelID))
	if err != nil {
		return "", "", err
	}
	return state, modelID, nil
}

// Compile-time check: *questEmbedProvider satisfies the questEmbedResolver
// interface declared in internal/quest/search_cmd.go. The quest package
// type-switches d.Embed against that interface; keeping this assertion here
// keeps the contract visible at the wiring call site without a reverse import.
var _ interface {
	ResolveQuestEmbedDeps(ctx context.Context) *quest.QuestEmbedDeps
} = (*questEmbedProvider)(nil)
