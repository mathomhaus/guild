package lore_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mathomhaus/guild/internal/command"
	"github.com/mathomhaus/guild/internal/lore"
	"github.com/spf13/cobra"
)

// TestAllCommandSpecs_ArgFieldKindAlignment is the lore-side sibling of
// the quest-package test. Same rationale: catch ArgSpec.Type ↔ field
// kind mismatches at init time instead of runtime setField panics.
func TestAllCommandSpecs_ArgFieldKindAlignment(t *testing.T) {
	cases := []struct {
		name      string
		args      []command.ArgSpec
		inputType reflect.Type
	}{
		{"lore.OathCommand", lore.OathCommand.Args, reflect.TypeFor[lore.OathInput]()},
		{"lore.DossierCommand", lore.DossierCommand.Args, reflect.TypeFor[lore.DossierInput]()},
		{"lore.SealCommand", lore.SealCommand.Args, reflect.TypeFor[lore.SealInput]()},
		{"lore.LinkCommand", lore.LinkCommand.Args, reflect.TypeFor[lore.LinkInput]()},
		{"lore.ReforgeCommand", lore.ReforgeCommand.Args, reflect.TypeFor[lore.ReforgeInput]()},
		{"lore.InscribeCommand", lore.InscribeCommand.Args, reflect.TypeFor[lore.InscribeInput]()},
		{"lore.UpdateCommand", lore.UpdateCommand.Args, reflect.TypeFor[lore.UpdateInput]()},
		{"lore.CatalogCommand", lore.CatalogCommand.Args, reflect.TypeFor[lore.CatalogInput]()},
		{"lore.EchoesCommand", lore.EchoesCommand.Args, reflect.TypeFor[lore.EchoesInput]()},
		{"lore.WhispersCommand", lore.WhispersCommand.Args, reflect.TypeFor[lore.WhispersInput]()},
		{"lore.ListCommand", lore.ListCommand.Args, reflect.TypeFor[lore.ListInput]()},
		{"lore.InquestCommand", lore.InquestCommand.Args, reflect.TypeFor[lore.InquestInput]()},
		{"lore.MeldCommand", lore.MeldCommand.Args, reflect.TypeFor[lore.MeldInput]()},
		{"lore.CommuneCommand", lore.CommuneCommand.Args, reflect.TypeFor[lore.CommuneInput]()},
		{"lore.AppraiseCommand", lore.AppraiseCommand.Args, reflect.TypeFor[lore.AppraiseInput]()},
		{"lore.StudyCommand", lore.StudyCommand.Args, reflect.TypeFor[lore.StudyInput]()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := command.ValidateSpec(tc.args, tc.inputType); err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		})
	}
}

func TestInscribeCommand_ExposesStrictProjectOnCLIAndMCP(t *testing.T) {
	parent := &cobra.Command{Use: "lore"}
	lore.InscribeCommand.BindCobra(parent, command.Deps{})
	sub := findSubcommand(parent, "inscribe")
	if sub == nil {
		t.Fatal("inscribe subcommand not registered")
	}
	if sub.Flags().Lookup("strict-project") == nil {
		t.Fatal("inscribe command missing --strict-project flag")
	}

	tool := lore.InscribeCommand.BuildMCPForTest(command.Deps{})
	buf, _ := json.Marshal(tool.InputSchema)
	schema := string(buf)
	if !strings.Contains(schema, `"strict_project"`) {
		t.Fatalf("lore_inscribe schema missing strict_project:\n%s", schema)
	}
}

func findSubcommand(parent *cobra.Command, name string) *cobra.Command {
	for _, cmd := range parent.Commands() {
		if cmd.Name() == name {
			return cmd
		}
	}
	return nil
}
