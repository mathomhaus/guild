package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// fakeEnv returns a getenv func backed by a map, so detection tests
// never read the real process environment (which may itself be an
// agent harness when a developer runs `make check` from one).
func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDetectAgentEnv(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"empty env", map[string]string{}, false},
		{"GUILD_AGENT=1", map[string]string{"GUILD_AGENT": "1"}, true},
		{"GUILD_AGENT=true case-insensitive", map[string]string{"GUILD_AGENT": "TRUE"}, true},
		{"GUILD_AGENT identity name is not a toggle", map[string]string{"GUILD_AGENT": "alice"}, false},
		{"GUILD_AGENT=0 forces off over harness marker", map[string]string{"GUILD_AGENT": "0", "CLAUDECODE": "1"}, false},
		{"GUILD_AGENT=false forces off", map[string]string{"GUILD_AGENT": "false", "CURSOR_AGENT": "1"}, false},
		{"claude code marker", map[string]string{"CLAUDECODE": "1"}, true},
		{"codex sandbox marker", map[string]string{"CODEX_SANDBOX": "seatbelt"}, true},
		{"codex network marker", map[string]string{"CODEX_SANDBOX_NETWORK_DISABLED": "1"}, true},
		{"cursor agent marker", map[string]string{"CURSOR_AGENT": "1"}, true},
		{"marker explicitly zeroed", map[string]string{"CLAUDECODE": "0"}, false},
		{"marker explicitly falsed", map[string]string{"CODEX_SANDBOX": "false"}, false},
		{"identity plus marker still detects", map[string]string{"GUILD_AGENT": "alice", "CLAUDECODE": "1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectAgentEnv(fakeEnv(tc.env)); got != tc.want {
				t.Errorf("DetectAgentEnv(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestAgentEnvelope_SchemaStability locks the wire shape: exact key
// names, exact ordering as marshaled by encoding/json, and omission of
// empty optional fields. A diff here is a breaking change for agents
// parsing the envelope; treat the schema as append-only.
func TestAgentEnvelope_SchemaStability(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		got, err := json.Marshal(AgentEnvelope{
			OK:      true,
			Command: "quest_post",
			Output:  map[string]any{"id": "QUEST-1"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"ok":true,"command":"quest_post","output":{"id":"QUEST-1"}}`
		if string(got) != want {
			t.Errorf("success envelope = %s, want %s", got, want)
		}
	})
	t.Run("error with hint", func(t *testing.T) {
		got, err := json.Marshal(AgentEnvelope{
			OK:      false,
			Command: "quest_accept",
			Error:   "quest not found: QUEST-99",
			Hint:    "run 'guild quest list' to see open quests",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"ok":false,"command":"quest_accept","error":"quest not found: QUEST-99","hint":"run 'guild quest list' to see open quests"}`
		if string(got) != want {
			t.Errorf("error envelope = %s, want %s", got, want)
		}
	})
	t.Run("error without hint omits the field", func(t *testing.T) {
		got, err := json.Marshal(AgentEnvelope{OK: false, Command: "lore_seal", Error: "boom"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want := `{"ok":false,"command":"lore_seal","error":"boom"}`
		if string(got) != want {
			t.Errorf("error envelope = %s, want %s", got, want)
		}
	})
}

func TestWithHint(t *testing.T) {
	base := errors.New("project not registered")

	t.Run("error text passes through unchanged", func(t *testing.T) {
		err := WithHint(base, "run 'guild init' first")
		if err.Error() != base.Error() {
			t.Errorf("Error() = %q, want %q", err.Error(), base.Error())
		}
		if !errors.Is(err, base) {
			t.Error("WithHint broke the errors.Is chain")
		}
	})

	t.Run("hint recoverable through further wrapping", func(t *testing.T) {
		err := fmt.Errorf("resolve: %w", WithHint(base, "run 'guild init' first"))
		if got := AgentHintFromError(err); got != "run 'guild init' first" {
			t.Errorf("AgentHintFromError = %q, want the attached hint", got)
		}
	})

	t.Run("nil error and blank hint are no-ops", func(t *testing.T) {
		if WithHint(nil, "hint") != nil {
			t.Error("WithHint(nil, ...) should stay nil")
		}
		if err := WithHint(base, "  "); err != base {
			t.Error("WithHint with blank hint should return err unchanged")
		}
		if got := AgentHintFromError(base); got != "" {
			t.Errorf("AgentHintFromError(plain error) = %q, want empty", got)
		}
	})
}

func TestAgentReportedError(t *testing.T) {
	base := errors.New("boom")
	wrapped := &AgentReportedError{Err: base}
	if !IsAgentReported(wrapped) {
		t.Error("IsAgentReported(direct) = false, want true")
	}
	if !IsAgentReported(fmt.Errorf("outer: %w", wrapped)) {
		t.Error("IsAgentReported(wrapped) = false, want true")
	}
	if IsAgentReported(base) {
		t.Error("IsAgentReported(plain error) = true, want false")
	}
	if !errors.Is(wrapped, base) {
		t.Error("AgentReportedError broke the errors.Is chain")
	}
}

func TestCobraFlagValueType(t *testing.T) {
	cases := []struct {
		spec ArgSpec
		want string
	}{
		{ArgSpec{Type: ArgString}, "string"},
		{ArgSpec{Type: ArgBool}, "bool"},
		{ArgSpec{Type: ArgInt}, "int"},
		{ArgSpec{Type: ArgStringSlice}, "stringSlice"},
		{ArgSpec{Type: ArgStringSlice, Repeatable: true}, "stringArray"},
	}
	for _, tc := range cases {
		if got := cobraFlagValueType(tc.spec); got != tc.want {
			t.Errorf("cobraFlagValueType(%+v) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}
