package command

import "testing"

// TestCobraPositionalValidator_VariadicRequired keeps the
// historical behavior: a Required variadic positional demands at
// least one positional CLI arg.
func TestCobraPositionalValidator_VariadicRequired(t *testing.T) {
	args := []ArgSpec{
		{Name: "query", Kind: ArgPositional, Type: ArgString, Required: true, Variadic: true, Help: "q"},
	}
	v := cobraPositionalValidator(args)
	if err := v(nil, []string{}); err == nil {
		t.Fatal("expected error for missing required positional, got nil")
	}
	if err := v(nil, []string{"foo"}); err != nil {
		t.Fatalf("unexpected error with one positional: %v", err)
	}
}

// TestCobraPositionalValidator_VariadicOptional asserts the new
// affordance: a non-Required variadic positional accepts zero
// positional args so a sibling flag can supply the value.
func TestCobraPositionalValidator_VariadicOptional(t *testing.T) {
	args := []ArgSpec{
		{Name: "query", Kind: ArgPositional, Type: ArgString, Variadic: true, Help: "q"},
		{Name: "query_flag", CLIFlagName: "query", Short: "q", Kind: ArgFlag, Type: ArgString, CLIOnly: true, Help: "q via flag"},
	}
	v := cobraPositionalValidator(args)
	if err := v(nil, []string{}); err != nil {
		t.Fatalf("expected zero positionals to be allowed when Required=false: %v", err)
	}
	if err := v(nil, []string{"foo", "bar"}); err != nil {
		t.Fatalf("unexpected error with two positionals: %v", err)
	}
}
