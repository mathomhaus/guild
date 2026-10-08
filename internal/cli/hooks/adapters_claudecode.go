package hooks

// Production adapter wiring. Linking an adapter package into the build
// is what registers it (init-time self-registration, database/sql
// driver style). Each adapter gets its own one-line wiring file so
// adding a harness never edits a shared import block.

import (
	// Registers the "claude-code" adapter.
	_ "github.com/mathomhaus/guild/internal/hooks/adapters/claudecode"
)
