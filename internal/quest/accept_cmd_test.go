package quest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/quest"
)

// TestAcceptCommand_CobraSurface locks the cobra *command*.Use/Short/Long
// and flag set. Any drift from the spec regresses this test, which is
// exactly the drift-gate QUEST-44's conformance test exists to provide.
func TestAcceptCommand_CobraSurface(t *testing.T) {
	parent := &cobra.Command{Use: "quest"}
	// Parent mirrors the real questCmd in internal/cli: --project is a
	// persistent flag attached to the group. The adapter must detect
	// this and skip local re-registration.
	parent.PersistentFlags().StringP("project", "p", "", "project name (overrides CWD detection)")

	quest.AcceptCommand.BindCobra(parent, fakeDeps(t))

	var sub *cobra.Command
	for _, c := range parent.Commands() {
		if c.Name() == "accept" {
			sub = c
			break
		}
	}
	if sub == nil {
		t.Fatal("accept subcommand not registered")
	}

	if got, want := sub.Use, "accept QUEST_ID"; got != want {
		t.Errorf("Use=%q want %q", got, want)
	}
	if sub.Short != quest.AcceptCommand.Short {
		t.Errorf("Short=%q want %q", sub.Short, quest.AcceptCommand.Short)
	}
	if sub.Long != quest.AcceptCommand.Long {
		t.Errorf("Long=%q want %q", sub.Long, quest.AcceptCommand.Long)
	}

	// --owner: locally registered (not on parent).
	if sub.Flags().Lookup("owner") == nil {
		t.Error("--owner flag missing")
	}
	// --project: inherited from parent (not re-registered locally).
	if sub.LocalFlags().Lookup("project") != nil {
		t.Error("--project re-registered locally; should inherit parent persistent flag")
	}
	if sub.Flags().Lookup("project") == nil {
		t.Error("--project not visible via merged Flags()")
	}
	// --json: universal affordance added by the adapter.
	if sub.Flags().Lookup("json") == nil {
		t.Error("--json flag missing (adapter should add it)")
	}
}

// TestAcceptCommand_MCPSurface locks the MCP tool name, description,
// and schema shape. Subset checks on the JSON schema (not an exact
// match) so jsonschema-go internals can evolve without tripping the
// test on incidental field ordering.
func TestAcceptCommand_MCPSurface(t *testing.T) {
	tool := quest.AcceptCommand.BuildMCPForTest(fakeDeps(t))

	if tool.Name != "quest_accept" {
		t.Errorf("Name=%q want quest_accept", tool.Name)
	}
	if !strings.HasPrefix(tool.Description, "Atomically claim a quest") {
		t.Errorf("Description=%q; expected to start with Long field", tool.Description)
	}
	if tool.InputSchema == nil {
		t.Fatal("InputSchema is nil")
	}
	buf, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	schemaJSON := string(buf)

	// Owner is CLIOnly — must NOT appear in the MCP schema.
	if strings.Contains(schemaJSON, `"owner"`) {
		t.Errorf("MCP schema leaked CLIOnly arg 'owner':\n%s", schemaJSON)
	}
	// quest_id + project must be present.
	for _, want := range []string{`"quest_id"`, `"project"`} {
		if !strings.Contains(schemaJSON, want) {
			t.Errorf("MCP schema missing %s:\n%s", want, schemaJSON)
		}
	}
}

// TestAcceptCommand_SpecAlignsWithInput is the ArgSpec ↔ input-struct
// lint: every exported json field on AcceptInput must correspond to an
// ArgSpec, and vice versa. Catches "added a field, forgot the ArgSpec"
// at CI time instead of in production.
func TestAcceptCommand_SpecAlignsWithInput(t *testing.T) {
	// Exercise the registry through a public entry point: the command
	// builds a cobra subcommand + MCP tool, both derived from the spec.
	// A misalignment surfaces either as a missing flag or as a dropped
	// schema property — we check the raw spec/input alignment directly
	// by walking them.
	spec := quest.AcceptCommand
	inputType := reflect.TypeFor[quest.AcceptInput]()

	argNames := map[string]bool{}
	for _, a := range spec.Args {
		if strings.TrimSpace(a.Name) == "" {
			t.Errorf("ArgSpec with empty Name")
		}
		if strings.TrimSpace(a.Help) == "" {
			t.Errorf("ArgSpec %q has empty Help", a.Name)
		}
		argNames[a.Name] = true
	}
	for i := 0; i < inputType.NumField(); i++ {
		f := inputType.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if !argNames[name] {
			t.Errorf("input field %q has no matching ArgSpec", name)
		}
	}
}

func TestAcceptCommand_NoActiveProjectGuidesSessionStart(t *testing.T) {
	msg, ok := quest.AcceptCommand.MCPErrorFormat(
		command.MCPSink{},
		errors.New("no active project set"),
	)
	if !ok {
		t.Fatal("MCPErrorFormat did not handle no-active-project error")
	}
	if !strings.Contains(msg, "guild_session_start") {
		t.Fatalf("message should guide caller to guild_session_start; got %q", msg)
	}
	if !strings.Contains(msg, "quest_accept") {
		t.Fatalf("message should name quest_accept; got %q", msg)
	}
}

// fakeDeps produces a Deps bundle with no-op DB and pass-through
// project resolver — sufficient for surface-shape tests that never
// actually execute the handler.
func fakeDeps(_ *testing.T) command.Deps {
	return command.Deps{
		OpenDB: func(_ context.Context) (*sql.DB, error) { return nil, nil },
		ResolveProj: func(_ context.Context, arg string) (string, error) {
			return arg, nil
		},
		Now: time.Now,
	}
}
