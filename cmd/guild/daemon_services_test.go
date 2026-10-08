package main

import (
	"context"
	"io/fs"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/config"
	"github.com/mathomhaus/guild/internal/module"
)

// fixtureService is a no-op module.Service used to prove the daemon's
// service-collection respects the module toggle.
type fixtureService struct{ name string }

func (f fixtureService) Name() string                { return f.name }
func (f fixtureService) Start(context.Context) error { return nil }
func (f fixtureService) Stop(context.Context) error  { return nil }

// fixtureModule is a registered-but-off-by-default capability module that
// contributes one daemon Service. It models a future module shipping a
// loop, so enabledModuleServices can be proven to start it only when the
// operator enables the module.
type fixtureModule struct{}

func (fixtureModule) Name() string                            { return "phase3svcfixture" }
func (fixtureModule) DefaultEnabled() bool                    { return false }
func (fixtureModule) Commands() []command.Registrant          { return nil }
func (fixtureModule) Migrations() (fsys fs.FS, dbName string) { return nil, "" }
func (fixtureModule) Instructions() string                    { return "" }
func (fixtureModule) Services() []module.Service {
	return []module.Service{fixtureService{name: "phase3-fixture-loop"}}
}

func init() { module.Register(fixtureModule{}) }

// TestEnabledModuleServices_RespectsToggle is the daemon half of the
// ADR-006 Phase 3 toggle proof: a module's daemon Services are collected
// (and therefore started by the daemon's Run loop) ONLY when the module is
// enabled. Disabling it — or leaving it off by default — means its loop is
// never started. This pins the "disabled module's daemon loop not started"
// acceptance criterion without needing a real background loop in docker.
func TestEnabledModuleServices_RespectsToggle(t *testing.T) {
	hasFixtureLoop := func(svcs []module.Service) bool {
		for _, s := range svcs {
			if s.Name() == "phase3-fixture-loop" {
				return true
			}
		}
		return false
	}

	// Off by default (DefaultEnabled=false, no [modules] override): the
	// loop is absent.
	def := &config.Config{}
	if hasFixtureLoop(enabledModuleServices(def)) {
		t.Error("fixture module is off by default; its daemon loop must not be collected")
	}

	// Explicitly enabled: the loop is collected (the daemon would start it).
	on := &config.Config{Modules: config.ModulesConfig{"phase3svcfixture": true}}
	if !hasFixtureLoop(enabledModuleServices(on)) {
		t.Error("fixture module enabled via [modules]; its daemon loop must be collected")
	}

	// Explicitly disabled (belt and suspenders): still absent.
	off := &config.Config{Modules: config.ModulesConfig{"phase3svcfixture": false}}
	if hasFixtureLoop(enabledModuleServices(off)) {
		t.Error("fixture module disabled via [modules]; its daemon loop must not be collected")
	}

	// A core module disabled must not drop a co-enabled module's loop:
	// disabling lore while enabling the fixture still collects the loop.
	mixed := &config.Config{Modules: config.ModulesConfig{
		"lore":             false,
		"phase3svcfixture": true,
	}}
	if !hasFixtureLoop(enabledModuleServices(mixed)) {
		t.Error("disabling lore must not drop the co-enabled fixture's daemon loop")
	}
}
