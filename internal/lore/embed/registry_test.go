package embed

import (
	"context"
	"errors"
	"testing"
)

// TestRegistry_DefaultBackendRegistered asserts the bundled local BGE backend
// self-registered under its stable name and aliases via init(). This is the
// byte-identical default: an empty backend resolves here.
func TestRegistry_DefaultBackendRegistered(t *testing.T) {
	for _, name := range []string{LocalBGEBackend, "bge", "local"} {
		if !HasEmbedder(name) {
			t.Errorf("expected backend %q registered by init()", name)
		}
	}
	// Empty normalizes to LocalBGEBackend.
	if !HasEmbedder("") {
		t.Error("empty backend name must normalize to the default and report registered")
	}
	if !IsDefaultBackend("") || !IsDefaultBackend(LocalBGEBackend) {
		t.Error("empty and LocalBGEBackend must both be the default backend")
	}
	if IsDefaultBackend("openai") {
		t.Error("an alternate name must not be reported as the default backend")
	}
}

// TestRegistry_UnknownBackendErrors proves a typo'd backend name is a loud
// error from BuildEmbedder, never a silent fallback.
func TestRegistry_UnknownBackendErrors(t *testing.T) {
	_, err := BuildEmbedder(EmbedConfig{Backend: "does-not-exist"})
	if err == nil {
		t.Fatal("BuildEmbedder with an unregistered backend must return an error")
	}
}

// TestRegistry_RegisterPanicsOnEmpty/Dup/Nil guard the init-time programmer
// errors, matching the adapters/database-sql Register contract.
func TestRegistry_RegisterPanicsOnEmpty(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("RegisterEmbedder with empty name must panic")
		}
	}()
	RegisterEmbedder("", func(EmbedConfig) (Embedder, error) { return nil, nil })
}

func TestRegistry_RegisterPanicsOnNilFactory(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("RegisterEmbedder with nil factory must panic")
		}
	}()
	RegisterEmbedder("nilfactory", nil)
}

func TestRegistry_RegisterPanicsOnDuplicate(t *testing.T) {
	name := "dup-test-backend"
	RegisterEmbedder(name, func(EmbedConfig) (Embedder, error) { return NewNullEmbedder(), nil })
	defer func() {
		if recover() == nil {
			t.Error("RegisterEmbedder twice for the same name must panic")
		}
	}()
	RegisterEmbedder(name, func(EmbedConfig) (Embedder, error) { return NewNullEmbedder(), nil })
}

// TestRegistry_BuildAlternateBackend proves the selection seam: a registered
// alternate backend is returned by name through BuildEmbedder. Uses an
// in-process stub embedder so the test needs no network and no model assets.
func TestRegistry_BuildAlternateBackend(t *testing.T) {
	const alt = "stub-alt-test"
	RegisterEmbedder(alt, func(cfg EmbedConfig) (Embedder, error) {
		return &countingStubEmbedder{model: cfg.Model}, nil
	})

	emb, err := BuildEmbedder(EmbedConfig{Backend: alt, Model: "test-model"})
	if err != nil {
		t.Fatalf("BuildEmbedder(%q): %v", alt, err)
	}
	stub, ok := emb.(*countingStubEmbedder)
	if !ok {
		t.Fatalf("BuildEmbedder returned %T, want *countingStubEmbedder", emb)
	}
	if stub.model != "test-model" {
		t.Errorf("factory did not receive cfg.Model: got %q", stub.model)
	}
	if got := emb.Dimension(); got != Dim {
		t.Errorf("stub Dimension = %d, want %d", got, Dim)
	}
	vec, err := emb.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatalf("stub Embed: %v", err)
	}
	if len(vec) != Dim {
		t.Errorf("stub vector len = %d, want %d", len(vec), Dim)
	}
}

// TestRegistry_FactoryErrorPropagates verifies a factory error surfaces from
// BuildEmbedder rather than being swallowed.
func TestRegistry_FactoryErrorPropagates(t *testing.T) {
	const name = "erroring-backend-test"
	sentinel := errors.New("boom")
	RegisterEmbedder(name, func(EmbedConfig) (Embedder, error) { return nil, sentinel })
	_, err := BuildEmbedder(EmbedConfig{Backend: name})
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("BuildEmbedder must propagate the factory error; got %v", err)
	}
}

// countingStubEmbedder is a minimal in-process Embedder for selection tests.
// It emits a fixed unit vector and never touches the network or model assets.
type countingStubEmbedder struct {
	model string
}

func (s *countingStubEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]float32, Dim)
	out[0] = 1.0 // unit vector along the first axis; well-defined norm.
	return out, nil
}

func (s *countingStubEmbedder) Dimension() int { return Dim }
