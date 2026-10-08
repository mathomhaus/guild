// Package stub ships a minimal file-backed hook adapter used to test
// the registry plumbing and the shared merge/scan/atomic-write
// machinery end to end. It is intentionally NOT imported by any
// production package: only tests import it (the import side effect is
// the init() self-registration). Real harness adapters follow the same
// shape and land in their own packages.
package stub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mathomhaus/guild/internal/guildpath"
	"github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
)

// settingsFileName is the basename of the stub's settings file under
// ~/.guild/. Tests point HOME at a t.TempDir(), so nothing here ever
// touches a real install.
const settingsFileName = "stub-settings.json"

// hooksKey is the top-level settings key the hook events live under,
// matching the Claude Code shaped layout the shared helpers default to.
const hooksKey = "hooks"

// Adapter is the no-op-ish stub: identity Substitute, settings rendered
// as {"hooks": <abstract config>} via the shared helpers.
type Adapter struct{}

func init() { adapters.Register(Adapter{}) }

// Name implements adapters.Adapter.
func (Adapter) Name() string { return "stub" }

// Detect implements adapters.Adapter. The stub is always "present":
// detection gating is exercised by tests through fake adapters, and
// this package never ships in a production import graph.
func (Adapter) Detect() (bool, error) { return true, nil }

// SettingsPath implements adapters.Adapter.
func (Adapter) SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("stub: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".guild", settingsFileName), nil
}

// Substitute implements adapters.Adapter: the stub has no placeholders.
func (Adapter) Substitute(cmd string) string { return cmd }

// Install implements adapters.Adapter. First-time setup and sync are
// the same operation for the stub: an ownership-aware merge.
func (a Adapter) Install(base adapters.Config) error { return a.Sync(base) }

// Sync implements adapters.Adapter: merge the guild-owned groups into
// the settings file, preserving foreign content, and write atomically
// only when something changed.
func (a Adapter) Sync(base adapters.Config) error {
	path, err := a.SettingsPath()
	if err != nil {
		return err
	}
	// G304: path derives from the trusted home dir plus a compile-time
	// constant basename; no user-controlled input.
	raw, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stub: read %s: %w", path, err)
	}
	desired := hooks.ApplySubstitution(base, a.Substitute)
	merged, changed, err := hooks.MergeSettingsDoc(raw, desired, hooksKey)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := guildpath.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return hooks.WriteFileAtomic(path, merged, 0o600)
}

// Scan implements adapters.Adapter: flatten every hook in the settings
// file, guild-owned and foreign alike. Missing file means empty list.
func (a Adapter) Scan() ([]adapters.Hook, error) {
	path, err := a.SettingsPath()
	if err != nil {
		return nil, err
	}
	// G304: see Sync.
	raw, err := os.ReadFile(path) //nolint:gosec // trusted path; see note above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stub: read %s: %w", path, err)
	}
	return hooks.ScanSettingsDoc(raw, hooksKey)
}
