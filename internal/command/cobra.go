package command

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// ResetCobraSliceFlags walks every descendant command under root and
// resets its slice-valued flags (StringArray, StringSlice) to empty.
// pflag's Set implementation appends for slice types — re-running the
// same cobra tree in-process (tests using rootCmd.Execute multiple
// times) otherwise accumulates values across invocations. Production
// code hits this only once per process, so this helper is intended
// for test reset paths (resetQuestFlagState etc.).
func ResetCobraSliceFlags(root *cobra.Command) {
	if root == nil {
		return
	}
	for _, sub := range root.Commands() {
		sub.Flags().VisitAll(func(f *pflag.Flag) {
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace([]string{})
			}
		})
		ResetCobraSliceFlags(sub)
	}
}

// BindCobra attaches a cobra subcommand for c onto parent. MCPOnly
// commands return early (no CLI surface). Flags are registered AFTER
// AddCommand so inherited parent-persistent flags can be detected and
// the local registration skipped (preventing pflag redefinition panics).
func (c *Command[I, O]) BindCobra(parent *cobra.Command, d Deps) {
	if c.MCPOnly {
		return
	}
	cmd := &cobra.Command{
		Use:          cobraUse(c),
		Aliases:      c.CLIAliases,
		Short:        c.Short,
		Long:         c.Long,
		Args:         cobraPositionalValidator(c.Args),
		SilenceUsage: true,
	}
	parent.AddCommand(cmd)
	c.registerCobraFlags(cmd)
	cmd.RunE = c.cobraRunE(d)
}

func (c *Command[I, O]) registerCobraFlags(cmd *cobra.Command) {
	for i := range c.Args {
		a := c.Args[i]
		if a.MCPOnly || a.Kind == ArgPositional {
			continue
		}
		flagName := cobraFlagNameFor(a)
		// Skip if an ancestor already exposes this flag as persistent —
		// cobra merges inherited flags into cmd.Flags() at runtime.
		if cmd.InheritedFlags().Lookup(flagName) != nil {
			continue
		}
		registerCobraFlag(cmd, flagName, a)
	}
	// --json is universal: every generated CLI verb emits structured
	// output when set. Honors COMMAND_REGISTRY.md §7 locked decision.
	cmd.Flags().Bool("json", false, "emit structured JSON result instead of formatted text")
}

func (c *Command[I, O]) cobraRunE(d Deps) func(*cobra.Command, []string) error {
	return func(cc *cobra.Command, args []string) error {
		ctx := cc.Context()
		if ctx == nil {
			ctx = cc.Root().Context()
		}
		in, err := buildInputFromCobra[I](c.Args, args, cc)
		if err != nil {
			return err
		}
		out, herr := c.Handler(ctx, d, in)
		if herr != nil {
			return handleCobraError(cc, c, herr)
		}
		sink := CLISink{NoEmoji: cobraNoEmoji(cc)}
		jsonOut, _ := cc.Flags().GetBool("json")
		if jsonOut {
			buf, merr := json.Marshal(out)
			if merr != nil {
				return fmt.Errorf("marshal output: %w", merr)
			}
			_, _ = fmt.Fprintln(cc.OutOrStdout(), string(buf))
			return nil
		}
		if c.CLIFormat == nil {
			return fmt.Errorf("command %q: CLIFormat required (set MCPOnly=true to skip CLI)", c.Name)
		}
		body := c.CLIFormat(sink, out)
		_, _ = fmt.Fprintln(cc.OutOrStdout(), body)
		if c.CLIWarnings != nil {
			if w := c.CLIWarnings(sink, out); w != "" {
				_, _ = fmt.Fprint(cc.ErrOrStderr(), w)
			}
		}
		if notice, ok := c.CLIAliasDeprecations[cc.CalledAs()]; ok && notice != "" {
			_, _ = fmt.Fprintln(cc.ErrOrStderr(), notice)
		}
		return nil
	}
}

func cobraUse[I, O any](c *Command[I, O]) string {
	last := ""
	if len(c.CLIPath) > 0 {
		last = c.CLIPath[len(c.CLIPath)-1]
	}
	var positionals []string
	for _, a := range c.Args {
		if a.Kind == ArgPositional && !a.MCPOnly {
			token := strings.ToUpper(a.Name)
			if a.Variadic {
				token += "..."
			}
			positionals = append(positionals, token)
		}
	}
	if len(positionals) == 0 {
		return last
	}
	return last + " " + strings.Join(positionals, " ")
}

func countPositional(args []ArgSpec) int {
	n := 0
	for _, a := range args {
		if a.Kind == ArgPositional && !a.MCPOnly {
			n++
		}
	}
	return n
}

// cobraPositionalValidator returns the cobra args validator for the
// spec's positional args. If any positional is Variadic, the count is
// a minimum (cobra.MinimumNArgs). Otherwise exactly-N applies.
func cobraPositionalValidator(args []ArgSpec) cobra.PositionalArgs {
	n := countPositional(args)
	for _, a := range args {
		if a.Kind == ArgPositional && a.Variadic && !a.MCPOnly {
			return cobra.MinimumNArgs(n)
		}
	}
	return cobra.ExactArgs(n)
}

func cobraFlagName(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

// cobraFlagNameFor returns the CLI long-flag name for a spec — honors
// ArgSpec.CLIFlagName override, else falls back to dash-form of Name.
func cobraFlagNameFor(a ArgSpec) string {
	if a.CLIFlagName != "" {
		return a.CLIFlagName
	}
	return cobraFlagName(a.Name)
}

func registerCobraFlag(cmd *cobra.Command, flagName string, a ArgSpec) {
	switch a.Type {
	case ArgString:
		def, _ := a.Default.(string)
		if a.Short != "" {
			cmd.Flags().StringP(flagName, a.Short, def, a.Help)
		} else {
			cmd.Flags().String(flagName, def, a.Help)
		}
	case ArgBool:
		def, _ := a.Default.(bool)
		if a.Short != "" {
			cmd.Flags().BoolP(flagName, a.Short, def, a.Help)
		} else {
			cmd.Flags().Bool(flagName, def, a.Help)
		}
	case ArgStringSlice:
		def, _ := a.Default.([]string)
		if a.Repeatable {
			if a.Short != "" {
				cmd.Flags().StringArrayP(flagName, a.Short, def, a.Help)
			} else {
				cmd.Flags().StringArray(flagName, def, a.Help)
			}
		} else {
			if a.Short != "" {
				cmd.Flags().StringSliceP(flagName, a.Short, def, a.Help)
			} else {
				cmd.Flags().StringSlice(flagName, def, a.Help)
			}
		}
	case ArgInt:
		def, _ := a.Default.(int)
		if a.Short != "" {
			cmd.Flags().IntP(flagName, a.Short, def, a.Help)
		} else {
			cmd.Flags().Int(flagName, def, a.Help)
		}
	}
}

func buildInputFromCobra[I any](args []ArgSpec, positional []string, cc *cobra.Command) (I, error) {
	var in I
	v := reflect.ValueOf(&in).Elem()
	posIdx := 0
	for _, a := range args {
		if a.MCPOnly {
			continue
		}
		fv, ok := fieldByJSONName(v, a.Name)
		if !ok {
			continue
		}
		if a.Kind == ArgPositional {
			if posIdx >= len(positional) {
				continue
			}
			// Variadic slice: collect remaining positionals into a
			// []string — `epic NAME Q1 Q2 Q3` shape.
			if a.Variadic && a.Type == ArgStringSlice {
				rest := append([]string(nil), positional[posIdx:]...)
				posIdx = len(positional)
				if err := setField(fv, a.Type, rest); err != nil {
					return in, fmt.Errorf("arg %q: %w", a.Name, err)
				}
				continue
			}
			val := positional[posIdx]
			if a.Variadic {
				// Final variadic string absorbs all remaining positional
				// args joined by space — matches the CLI's long-standing
				// "quest journal QUEST-X multi word text" ergonomic.
				val = strings.Join(positional[posIdx:], " ")
				posIdx = len(positional)
			} else {
				posIdx++
			}
			if err := setField(fv, a.Type, val); err != nil {
				return in, fmt.Errorf("arg %q: %w", a.Name, err)
			}
			continue
		}
		val, err := readCobraFlag(cc, cobraFlagNameFor(a), a.Type)
		if err != nil {
			return in, fmt.Errorf("arg %q: %w", a.Name, err)
		}
		if val == nil {
			continue
		}
		if err := setField(fv, a.Type, val); err != nil {
			return in, fmt.Errorf("arg %q: %w", a.Name, err)
		}
	}
	return in, nil
}

func readCobraFlag(cc *cobra.Command, flagName string, t ArgType) (any, error) {
	if cc.Flags().Lookup(flagName) == nil {
		return nil, nil
	}
	switch t {
	case ArgString:
		return cc.Flags().GetString(flagName)
	case ArgBool:
		return cc.Flags().GetBool(flagName)
	case ArgStringSlice:
		// Try StringArray first, fall back to StringSlice. pflag stores
		// both under the same lookup but returns via different getters.
		if arr, err := cc.Flags().GetStringArray(flagName); err == nil {
			return arr, nil
		}
		return cc.Flags().GetStringSlice(flagName)
	case ArgInt:
		return cc.Flags().GetInt(flagName)
	}
	return nil, nil
}

func cobraNoEmoji(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	// Honor the flag directly when set. Also honor GUILD_NO_EMOJI=1 as
	// the env fallback — mirrors what internal/config.Load does for the
	// legacy non-registry commands. Cheap, no config-package dep.
	if f := cmd.Flags().Lookup("no-emoji"); f != nil {
		if b, _ := cmd.Flags().GetBool("no-emoji"); b {
			return true
		}
	}
	if v := os.Getenv("GUILD_NO_EMOJI"); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	return false
}

// handleCobraError applies CLIErrorFormat for bespoke error narration
// before falling back to cobra's default "Error: %v" rendering.
func handleCobraError[I, O any](cc *cobra.Command, c *Command[I, O], err error) error {
	if c.CLIErrorFormat != nil {
		sink := CLISink{NoEmoji: cobraNoEmoji(cc)}
		if msg, ok := c.CLIErrorFormat(sink, err); ok {
			_, _ = fmt.Fprintln(cc.ErrOrStderr(), strings.TrimRight(msg, "\n"))
			return err
		}
	}
	return err
}
