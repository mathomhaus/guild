package llm

// No-op LLM provider (ADR-006 Phase 4, deliverable 5).
//
// noopProvider is the default registered provider. It makes no network call
// and returns a deterministic stub completion, so a silent config never
// reaches a real LLM and the seam is exercisable in tests without any
// dependency. A future LLM-calling module replaces the default by naming a
// real provider in [provider].backend; this stub stays as the always-present
// fallback and the no-dependency test double.

import (
	"context"
	"fmt"
)

func init() {
	RegisterProvider(NoopProvider, newNoopProvider)
}

// newNoopProvider constructs the no-op provider. It captures the configured
// model only so Complete can echo it back; it never dials anything.
func newNoopProvider(cfg ProviderConfig) (Provider, error) {
	return &noopProvider{model: cfg.Model}, nil
}

// noopProvider satisfies Provider without any LLM dependency.
type noopProvider struct {
	model string
}

func (p *noopProvider) Name() string { return NoopProvider }

// Complete returns a deterministic marker response. Honors ctx cancellation so
// the seam's cancellation contract is real even for the stub. The model
// reported is the per-call override, then the configured model, then empty.
func (p *noopProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return CompletionResponse{}, err
	}
	model := req.Model
	if model == "" {
		model = p.model
	}
	return CompletionResponse{
		Text:  fmt.Sprintf("[noop] %s", req.Prompt),
		Model: model,
	}, nil
}
