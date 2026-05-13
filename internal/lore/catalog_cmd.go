package lore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mathomhaus/guild/internal/command"
)

type CatalogInput struct {
	Dir     string `json:"dir" jsonschema:"directory to walk for .md files"`
	Topic   string `json:"topic,omitempty" jsonschema:"override per-file topic; default: file stem"`
	Kind    string `json:"kind,omitempty" jsonschema:"override kind (default inferred from path)"`
	Tags    string `json:"tags,omitempty" jsonschema:"comma-separated tags applied to all imported entries"`
	Project string `json:"project,omitempty"`
}

type CatalogCmdOutput struct {
	Result *CatalogResult `json:"result"`
}

var CatalogCommand = &command.Command[CatalogInput, CatalogCmdOutput]{
	Name:       "lore_catalog",
	CLIPath:    []string{"lore", "catalog"},
	CLIAliases: []string{"migrate"},
	Short:      "bulk-import .md files as lore entries",
	Long:       "Bulk-import .md files under DIR as lore entries. Idempotent on re-runs.",
	Args: []command.ArgSpec{
		{Name: "dir", Kind: command.ArgPositional, Type: command.ArgString, Required: true, Help: "directory to walk for .md files"},
		{Name: "topic", Kind: command.ArgFlag, Type: command.ArgString, Help: "override per-file topic; default: file stem"},
		{Name: "kind", Kind: command.ArgFlag, Type: command.ArgString, Help: "override kind"},
		{Name: "tags", Kind: command.ArgFlag, Type: command.ArgString, Help: "comma-separated tags"},
		{Name: "project", Short: "p", Kind: command.ArgFlag, Type: command.ArgString, Help: "project override"},
	},
	Handler: func(ctx context.Context, d command.Deps, in CatalogInput) (CatalogCmdOutput, error) {
		if strings.TrimSpace(in.Dir) == "" {
			return CatalogCmdOutput{}, errors.New("dir required")
		}
		if in.Kind != "" {
			valid := false
			for _, k := range AllKinds() {
				if Kind(in.Kind) == k {
					valid = true
					break
				}
			}
			if !valid {
				return CatalogCmdOutput{}, fmt.Errorf("invalid --kind %q: valid kinds are: %s", in.Kind, strings.Join(func() []string {
					kinds := make([]string, len(AllKinds()))
					for i, k := range AllKinds() {
						kinds[i] = string(k)
					}
					return kinds
				}(), ", "))
			}
		}
		db, err := d.OpenDB(ctx)
		if err != nil {
			return CatalogCmdOutput{}, err
		}
		defer func() { _ = db.Close() }()
		pid, err := d.ResolveProj(ctx, in.Project)
		if err != nil {
			return CatalogCmdOutput{}, err
		}
		res, err := Catalog(ctx, db, &CatalogParams{
			Dir:       in.Dir,
			ProjectID: pid,
			Topic:     in.Topic,
			Kind:      Kind(in.Kind),
			Tags:      in.Tags,
		})
		if err != nil {
			return CatalogCmdOutput{}, err
		}
		return CatalogCmdOutput{Result: res}, nil
	},
	CLIFormat: func(s command.CLISink, o CatalogCmdOutput) string {
		return strings.TrimRight(s.Line("📚", "[catalog]",
			fmt.Sprintf("cataloged: imported=%d skipped=%d", o.Result.Imported, o.Result.Skipped)), "\n")
	},
	MCPFormat: func(s command.MCPSink, o CatalogCmdOutput) string {
		return strings.TrimRight(s.Line("📚", "[catalog]",
			fmt.Sprintf("cataloged: imported=%d skipped=%d", o.Result.Imported, o.Result.Skipped)), "\n")
	},
}
