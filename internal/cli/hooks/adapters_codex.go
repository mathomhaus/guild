package hooks

// Production adapter wiring; see adapters_claudecode.go for the
// pattern (init-time self-registration, one wiring file per adapter).

import (
	// Registers the "codex" adapter.
	_ "github.com/mathomhaus/guild/internal/hooks/adapters/codex"
)
