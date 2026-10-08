package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hookcfg "github.com/mathomhaus/guild/internal/hooks"
	"github.com/mathomhaus/guild/internal/hooks/adapters"
	"github.com/mathomhaus/guild/internal/install"
)

// renderFake is a file-backed adapter that, like the Codex adapter,
// cannot represent the base config one-to-one: it drops PreCompact and
// narrows the SessionStart matcher to startup|resume. It implements
// adapters.Renderer so the CLI's desired-state computation must agree
// with what Sync writes.
type renderFake struct {
	dir string
}

func (renderFake) Name() string                 { return "render-fake" }
func (renderFake) Detect() (bool, error)        { return true, nil }
func (renderFake) Substitute(cmd string) string { return cmd }

func (f renderFake) SettingsPath() (string, error) {
	return filepath.Join(f.dir, "render-fake.json"), nil
}

func (renderFake) Render(cfg adapters.Config) (adapters.Config, error) {
	out := make(adapters.Config, len(cfg))
	for ev, groups := range cfg {
		if ev == "PreCompact" {
			continue
		}
		gs := make([]hookcfg.Group, 0, len(groups))
		for _, g := range groups {
			matcher := g.Matcher
			if ev == "SessionStart" && matcher != "" {
				matcher = "startup|resume"
			}
			if ev == "UserPromptSubmit" && matcher != "" {
				return nil, errors.New("render-fake: UserPromptSubmit does not support a matcher")
			}
			gs = append(gs, hookcfg.Group{Matcher: matcher, Hooks: g.Hooks})
		}
		if len(gs) > 0 {
			out[ev] = gs
		}
	}
	return out, nil
}

func (f renderFake) Install(base adapters.Config) error { return f.Sync(base) }

func (f renderFake) Sync(base adapters.Config) error {
	path, err := f.SettingsPath()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // t.TempDir path
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	desired, err := f.Render(hookcfg.ApplySubstitution(base, f.Substitute))
	if err != nil {
		return err
	}
	merged, changed, err := hookcfg.MergeSettingsDoc(raw, desired, "hooks")
	if err != nil || !changed {
		return err
	}
	return hookcfg.WriteFileAtomic(path, merged, 0o600)
}

func (f renderFake) Scan() ([]adapters.Hook, error) {
	path, err := f.SettingsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // t.TempDir path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return hookcfg.ScanSettingsDoc(raw, "hooks")
}

func renderDeps(t *testing.T, out *bytes.Buffer) (deps, renderFake) {
	t.Helper()
	fake := renderFake{dir: t.TempDir()}
	d := deps{
		adapters: []adapters.Adapter{fake},
		clients:  []install.Client{{Name: "Render Fake", CLIProbe: "go"}},
		out:      out,
	}
	return d, fake
}

// TestRenderer_InstallThenInSync: after install, list/sync must report
// in-sync even though the written file differs from the raw base
// config (PreCompact dropped, matcher narrowed). Before the Renderer
// seam this reported perpetual drift.
func TestRenderer_InstallThenInSync(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	d, fake := renderDeps(t, &out)

	if err := runInstall(d, "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(fake.dir, "render-fake.json"))
	if err != nil {
		t.Fatalf("settings not written: %v", err)
	}
	if strings.Contains(string(raw), "PreCompact") {
		t.Errorf("rendered settings still carry PreCompact:\n%s", raw)
	}

	out.Reset()
	if err := runSync(d, false); err != nil {
		t.Fatalf("runSync: %v", err)
	}
	if !strings.Contains(out.String(), "render-fake: in sync (no-op)") {
		t.Errorf("sync after install must be in sync, got:\n%s", out.String())
	}

	out.Reset()
	if err := runList(d, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	if !strings.Contains(out.String(), "in-sync") {
		t.Errorf("list must report in-sync, got:\n%s", out.String())
	}
}

// TestRenderer_InvalidBaseSurfacesError: a base config the renderer
// rejects fails sync loudly instead of writing a broken settings file.
func TestRenderer_InvalidBaseSurfacesError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	d, fake := renderDeps(t, &out)

	// Install once so targetState takes the scan/desired comparison
	// path on the next run.
	if err := runInstall(d, "", false); err != nil {
		t.Fatalf("runInstall: %v", err)
	}

	bad := hookcfg.DefaultBase()
	bad["UserPromptSubmit"][0].Matcher = "oops"
	if err := hookcfg.SaveBase(bad); err != nil {
		t.Fatalf("SaveBase: %v", err)
	}

	err := runSync(d, false)
	if err == nil {
		t.Fatal("runSync accepted a base config the renderer rejects")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("render desired state for %s", fake.Name())) {
		t.Errorf("error %q does not point at the renderer", err)
	}
}
