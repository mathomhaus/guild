package llm

import (
	"context"
	"errors"
	"testing"
)

// TestProvider_DefaultRegistered asserts the no-op provider self-registered
// via init() and is the default (empty backend resolves to it).
func TestProvider_DefaultRegistered(t *testing.T) {
	if !HasProvider(NoopProvider) {
		t.Errorf("expected %q provider registered by init()", NoopProvider)
	}
	if !HasProvider("") {
		t.Error("empty backend must normalize to the default provider")
	}
	if !IsDefaultProvider("") || !IsDefaultProvider(NoopProvider) {
		t.Error("empty and NoopProvider must both be the default provider")
	}
	if IsDefaultProvider("openai") {
		t.Error("an alternate name must not be reported as the default provider")
	}
}

// TestProvider_DefaultMakesNoCall proves the default provider returns a
// deterministic stub without any network dependency.
func TestProvider_DefaultMakesNoCall(t *testing.T) {
	p, err := BuildProvider(ProviderConfig{}) // empty -> noop
	if err != nil {
		t.Fatalf("BuildProvider default: %v", err)
	}
	if p.Name() != NoopProvider {
		t.Errorf("default provider Name = %q, want %q", p.Name(), NoopProvider)
	}
	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "[noop] hi" {
		t.Errorf("noop Complete Text = %q, want %q", resp.Text, "[noop] hi")
	}
}

// TestProvider_UnknownBackendErrors proves a typo'd provider name is a loud
// error, never a silent fallback.
func TestProvider_UnknownBackendErrors(t *testing.T) {
	if _, err := BuildProvider(ProviderConfig{Backend: "nope"}); err == nil {
		t.Fatal("BuildProvider with an unregistered backend must error")
	}
}

// TestProvider_BuildAlternate proves the selection seam works for an alternate
// registered provider, with the configured model threaded through.
func TestProvider_BuildAlternate(t *testing.T) {
	const alt = "stub-provider-test"
	RegisterProvider(alt, func(cfg ProviderConfig) (Provider, error) {
		return &echoProvider{name: alt, model: cfg.Model}, nil
	})
	p, err := BuildProvider(ProviderConfig{Backend: alt, Model: "m-1"})
	if err != nil {
		t.Fatalf("BuildProvider(%q): %v", alt, err)
	}
	if p.Name() != alt {
		t.Errorf("Name = %q, want %q", p.Name(), alt)
	}
	resp, err := p.Complete(context.Background(), CompletionRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Model != "m-1" {
		t.Errorf("configured model not threaded: got %q", resp.Model)
	}
}

func TestProvider_RegisterPanics(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("empty name must panic")
			}
		}()
		RegisterProvider("", func(ProviderConfig) (Provider, error) { return nil, nil })
	})
	t.Run("nil", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("nil factory must panic")
			}
		}()
		RegisterProvider("nilfac-test", nil)
	})
	t.Run("dup", func(t *testing.T) {
		RegisterProvider("dup-prov-test", func(ProviderConfig) (Provider, error) { return &echoProvider{}, nil })
		defer func() {
			if recover() == nil {
				t.Error("duplicate name must panic")
			}
		}()
		RegisterProvider("dup-prov-test", func(ProviderConfig) (Provider, error) { return &echoProvider{}, nil })
	})
}

func TestProvider_FactoryErrorPropagates(t *testing.T) {
	const name = "erroring-prov-test"
	sentinel := errors.New("boom")
	RegisterProvider(name, func(ProviderConfig) (Provider, error) { return nil, sentinel })
	if _, err := BuildProvider(ProviderConfig{Backend: name}); err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("factory error must propagate; got %v", err)
	}
}

// echoProvider is a minimal in-process Provider for selection tests.
type echoProvider struct {
	name  string
	model string
}

func (e *echoProvider) Name() string { return e.name }
func (e *echoProvider) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	if err := ctx.Err(); err != nil {
		return CompletionResponse{}, err
	}
	m := req.Model
	if m == "" {
		m = e.model
	}
	return CompletionResponse{Text: req.Prompt, Model: m}, nil
}
