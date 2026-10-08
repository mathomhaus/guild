package mcp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mathomhaus/guild/internal/lore/embed"
	"github.com/mathomhaus/guild/internal/storage"
)

var recoveryBackendSequence atomic.Uint64

func recoveryProvider(t *testing.T, factory embed.EmbedderFactory) (provider *embedProvider, dbPath string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider.db")
	seedLoreDB(t, path)
	const model = "test-recovery-model"
	flipMeta(t, path, "embedder_state", "enabled")
	flipMeta(t, path, "embedder_model_id", model)
	name := fmt.Sprintf("test-provider-recovery-%d", recoveryBackendSequence.Add(1))
	embed.RegisterEmbedder(name, factory)
	p := newEmbedProvider(func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, path) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.backend, p.model = name, model
	// This test owns only its isolated databases; background auto-backfill
	// must not open process-default storage or outlive the test.
	p.backfill = &backfillGate{}
	p.backfill.once.Do(func() {})
	return p, path
}

func expireLoreRetry(p *embedProvider) {
	p.mu.Lock()
	p.retryAfter = time.Now().Add(-time.Second)
	p.mu.Unlock()
}

func expireQuestRetry(p *questEmbedProvider) {
	p.mu.Lock()
	p.retryAfter = time.Now().Add(-time.Second)
	p.mu.Unlock()
}

func TestEmbedProvider_RetriesTransientInitialization(t *testing.T) {
	var attempts atomic.Int64
	p, path := recoveryProvider(t, func(embed.EmbedConfig) (embed.Embedder, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("temporary backend construction failure")
		}
		return embed.NewDeterministicEmbedder(), nil
	})
	ctx := context.Background()
	if got := p.ResolveEmbedDeps(ctx); got != nil {
		t.Fatalf("first failed construction returned %+v", got)
	}
	if got := p.ResolveEmbedDeps(ctx); got != nil || attempts.Load() != 1 {
		t.Fatal("failed provider retried before its cooldown")
	}
	expireLoreRetry(p)
	good := p.ResolveEmbedDeps(ctx)
	if good == nil || !good.Enabled() || attempts.Load() != 2 {
		t.Fatalf("enabled provider remained trapped in nil cache: deps=%+v attempts=%d", good, attempts.Load())
	}
	if again := p.ResolveEmbedDeps(ctx); again != good || attempts.Load() != 2 {
		t.Fatal("successful provider was reconstructed")
	}
	flipMeta(t, path, "embedder_state", "disabled")
	if p.ResolveEmbedDeps(ctx) != nil {
		t.Fatal("explicit disabled state reused enabled dependencies")
	}
	expireLoreRetry(p)
	if p.ResolveEmbedDeps(ctx) != nil || attempts.Load() != 2 {
		t.Fatal("disabled state retried backend construction")
	}
}

func TestEmbedProvider_SerializesColdConstruction(t *testing.T) {
	var attempts atomic.Int64
	entered, release := make(chan struct{}), make(chan struct{})
	p, _ := recoveryProvider(t, func(embed.EmbedConfig) (embed.Embedder, error) {
		if attempts.Add(1) == 1 {
			close(entered)
		}
		<-release
		return embed.NewDeterministicEmbedder(), nil
	})
	const workers = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan any, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- p.ResolveEmbedDeps(context.Background())
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("backend construction did not start")
	}
	// Keep the constructor blocked while overlapping tool calls resolve.
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	var first any
	for result := range results {
		if first == nil {
			first = result
		}
		if result != first {
			t.Fatal("concurrent callers received different resource caches")
		}
	}
	if p.cached == nil || attempts.Load() != 1 {
		t.Fatalf("duplicate cold constructors: attempts=%d cached=%+v", attempts.Load(), p.cached)
	}
}

func TestQuestEmbedProvider_RetriesTransientFailures(t *testing.T) {
	for _, dependencyFailure := range []bool{true, false} {
		t.Run(fmt.Sprintf("lore_failure=%v", dependencyFailure), func(t *testing.T) {
			var attempts atomic.Int64
			loreProvider, path := recoveryProvider(t, func(embed.EmbedConfig) (embed.Embedder, error) {
				if attempts.Add(1) == 1 && dependencyFailure {
					return nil, errors.New("temporary lore runtime failure")
				}
				return embed.NewDeterministicEmbedder(), nil
			})
			flipMeta(t, path, "quest.embedder_state", "enabled")
			flipMeta(t, path, "quest.embedder_model_id", loreProvider.model)
			var failOpen atomic.Bool
			failOpen.Store(!dependencyFailure)
			p := newQuestEmbedProvider(loreProvider, func(ctx context.Context) (*sql.DB, error) {
				if attempts.Load() > 0 && failOpen.Swap(false) {
					return nil, errors.New("temporary quest storage failure")
				}
				return storage.Open(ctx, path)
			}, loreProvider.logger)
			ctx := context.Background()
			if got := p.ResolveQuestEmbedDeps(ctx); got != nil {
				t.Fatalf("first failed construction returned %+v", got)
			}
			if p.ResolveQuestEmbedDeps(ctx) != nil {
				t.Fatal("failed provider ignored retry cooldown")
			}
			expireLoreRetry(loreProvider)
			expireQuestRetry(p)
			good := p.ResolveQuestEmbedDeps(ctx)
			if good == nil || !good.Enabled() {
				t.Fatal("quest provider remained trapped in nil cache")
			}
			if again := p.ResolveQuestEmbedDeps(ctx); again != good {
				t.Fatal("successful quest provider was reconstructed")
			}
			flipMeta(t, path, "quest.embedder_state", "disabled")
			if p.ResolveQuestEmbedDeps(ctx) != nil {
				t.Fatal("disabled quest provider remained active")
			}
		})
	}
}

func TestQuestEmbedProvider_SerializesColdConstruction(t *testing.T) {
	var attempts atomic.Int64
	loreProvider, path := recoveryProvider(t, func(embed.EmbedConfig) (embed.Embedder, error) {
		attempts.Add(1)
		return embed.NewDeterministicEmbedder(), nil
	})
	ctx := context.Background()
	if loreProvider.ResolveEmbedDeps(ctx) == nil {
		t.Fatal("lore dependency failed to initialize")
	}
	flipMeta(t, path, "quest.embedder_state", "enabled")
	flipMeta(t, path, "quest.embedder_model_id", loreProvider.model)
	p := newQuestEmbedProvider(loreProvider, func(ctx context.Context) (*sql.DB, error) { return storage.Open(ctx, path) }, loreProvider.logger)
	const workers = 24
	start := make(chan struct{})
	results := make(chan any, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- p.ResolveQuestEmbedDeps(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first any
	for result := range results {
		if first == nil {
			first = result
		}
		if result != first {
			t.Fatal("concurrent quest callers received duplicate index caches")
		}
	}
	if p.cached == nil || attempts.Load() != 1 {
		t.Fatalf("quest cache/dependency initialization mismatch: attempts=%d cached=%+v", attempts.Load(), p.cached)
	}
}
