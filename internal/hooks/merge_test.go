package hooks

import (
	"bytes"
	"encoding/json"
	"testing"
)

// --- ownership detection -------------------------------------------------

func TestCommandIsGuild(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"guild", true},
		{"guild quest brief --auto", true},
		{"guild lore appraise --inject --from-stdin-json", true},
		{"guildctl run", false},
		{"/usr/local/bin/guild quest brief", false},
		{"", false},
		{" guild quest brief", false},
		{"my-guild thing", false},
	}
	for _, c := range cases {
		if got := CommandIsGuild(c.cmd); got != c.want {
			t.Errorf("CommandIsGuild(%q) = %v; want %v", c.cmd, got, c.want)
		}
	}
}

func TestGroupIsGuildOwned(t *testing.T) {
	guild := Command{Type: "command", Command: "guild quest brief --auto"}
	foreign := Command{Type: "command", Command: "other-tool sync"}

	cases := []struct {
		name  string
		group Group
		want  bool
	}{
		{"all guild", Group{Hooks: []Command{guild, {Type: "command", Command: "guild lore appraise --inject --from-stdin-json"}}}, true},
		{"all foreign", Group{Hooks: []Command{foreign}}, false},
		{"mixed is foreign", Group{Hooks: []Command{guild, foreign}}, false},
		{"empty is foreign", Group{}, false},
	}
	for _, c := range cases {
		if got := GroupIsGuildOwned(c.group); got != c.want {
			t.Errorf("%s: GroupIsGuildOwned = %v; want %v", c.name, got, c.want)
		}
	}
}

// --- merge ---------------------------------------------------------------

// mustMerge wraps MergeSettingsDoc for tests.
func mustMerge(t *testing.T, raw []byte, desired Config) ([]byte, bool) {
	t.Helper()
	out, changed, err := MergeSettingsDoc(raw, desired, "hooks")
	if err != nil {
		t.Fatalf("MergeSettingsDoc: %v", err)
	}
	return out, changed
}

// TestMerge_CleanInstall: empty document gains the full desired config.
func TestMerge_CleanInstall(t *testing.T) {
	out, changed := mustMerge(t, nil, DefaultBase())
	if !changed {
		t.Error("changed = false on clean install; want true")
	}
	hs, err := ScanSettingsDoc(out, "hooks")
	if err != nil {
		t.Fatalf("ScanSettingsDoc: %v", err)
	}
	if len(hs) != 3 {
		t.Fatalf("scanned %d hooks; want 3", len(hs))
	}
	for _, h := range hs {
		if !h.GuildOwned {
			t.Errorf("hook %q not guild-owned after clean install", h.Command)
		}
	}
}

// TestMerge_Idempotent: merging the same desired config twice changes
// nothing the second time.
func TestMerge_Idempotent(t *testing.T) {
	first, _ := mustMerge(t, nil, DefaultBase())
	second, changed := mustMerge(t, first, DefaultBase())
	if changed {
		t.Error("changed = true on second merge; want false (idempotent)")
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second merge altered bytes:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// TestMerge_ForeignFieldsPreserved: unknown top-level fields, foreign
// hook groups (with unknown group-level fields), and foreign events all
// survive the merge untouched.
func TestMerge_ForeignFieldsPreserved(t *testing.T) {
	raw := []byte(`{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "other-tool prime"}], "vendor_field": {"keep": true}}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "security-scan"}]}
    ]
  }
}`)
	out, changed := mustMerge(t, raw, DefaultBase())
	if !changed {
		t.Fatal("changed = false; want true (guild hooks were added)")
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse merged doc: %v", err)
	}
	if string(doc["model"]) != `"opus"` {
		t.Errorf("top-level model field altered: %s", doc["model"])
	}
	if _, ok := doc["permissions"]; !ok {
		t.Error("top-level permissions field dropped")
	}

	var events map[string][]json.RawMessage
	if err := json.Unmarshal(doc["hooks"], &events); err != nil {
		t.Fatalf("parse hooks section: %v", err)
	}

	// Foreign SessionStart group survives at position 0 with its
	// unknown vendor_field; the guild group is appended after it.
	ss := events["SessionStart"]
	if len(ss) != 2 {
		t.Fatalf("SessionStart has %d groups; want 2 (foreign + guild)", len(ss))
	}
	var foreignGroup map[string]json.RawMessage
	if err := json.Unmarshal(ss[0], &foreignGroup); err != nil {
		t.Fatal(err)
	}
	if _, ok := foreignGroup["vendor_field"]; !ok {
		t.Error("vendor_field dropped from foreign group")
	}

	// Foreign-only event untouched.
	if len(events["PreToolUse"]) != 1 {
		t.Errorf("PreToolUse has %d groups; want 1", len(events["PreToolUse"]))
	}
}

// TestMerge_ReplacesGuildOwnedInPlace: an existing guild-owned group is
// rewritten in its original position, foreign neighbours keep theirs.
func TestMerge_ReplacesGuildOwnedInPlace(t *testing.T) {
	raw := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "guild quest brief"}]},
      {"matcher": "resume", "hooks": [{"type": "command", "command": "other-tool prime"}]}
    ]
  }
}`)
	desired := Config{
		"SessionStart": {{
			Matcher: "startup|resume|clear|compact",
			Hooks:   []Command{{Type: "command", Command: "guild quest brief --auto"}},
		}},
	}
	out, changed := mustMerge(t, raw, desired)
	if !changed {
		t.Fatal("changed = false; want true")
	}

	var doc struct {
		Hooks map[string][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	ss := doc.Hooks["SessionStart"]
	if len(ss) != 2 {
		t.Fatalf("SessionStart has %d groups; want 2", len(ss))
	}
	if got := ss[0].Hooks[0].Command; got != "guild quest brief --auto" {
		t.Errorf("position 0 command = %q; want updated guild command", got)
	}
	if got := ss[0].Matcher; got != "startup|resume|clear|compact" {
		t.Errorf("position 0 matcher = %q; want updated matcher", got)
	}
	if got := ss[1].Hooks[0].Command; got != "other-tool prime" {
		t.Errorf("position 1 command = %q; want untouched foreign command", got)
	}
}

// TestMerge_MixedGroupLeftUntouched: a group mixing guild and foreign
// commands is conservative-foreign; the desired guild group is appended
// rather than rewriting it.
func TestMerge_MixedGroupLeftUntouched(t *testing.T) {
	raw := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [
        {"type": "command", "command": "guild quest brief --auto"},
        {"type": "command", "command": "other-tool prime"}
      ]}
    ]
  }
}`)
	desired := Config{
		"SessionStart": {{
			Matcher: "startup|resume|clear|compact",
			Hooks:   []Command{{Type: "command", Command: "guild quest brief --auto"}},
		}},
	}
	out, _ := mustMerge(t, raw, desired)

	var doc struct {
		Hooks map[string][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	ss := doc.Hooks["SessionStart"]
	if len(ss) != 2 {
		t.Fatalf("SessionStart has %d groups; want 2 (mixed preserved + guild appended)", len(ss))
	}
	if len(ss[0].Hooks) != 2 {
		t.Errorf("mixed group was rewritten: %d hooks; want 2", len(ss[0].Hooks))
	}
	if ss[1].Matcher != "startup|resume|clear|compact" {
		t.Errorf("appended group matcher = %q; want desired matcher", ss[1].Matcher)
	}
}

// TestMerge_DropsStaleGuildGroups: guild-owned groups beyond the
// desired count, and guild-owned groups under events no longer in the
// base, are removed; foreign content stays.
func TestMerge_DropsStaleGuildGroups(t *testing.T) {
	raw := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup", "hooks": [{"type": "command", "command": "guild quest brief --auto"}]},
      {"matcher": "resume", "hooks": [{"type": "command", "command": "guild old-verb"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "guild retired-hook"}]},
      {"hooks": [{"type": "command", "command": "other-tool cleanup"}]}
    ]
  }
}`)
	desired := Config{
		"SessionStart": {{
			Matcher: "startup|resume|clear|compact",
			Hooks:   []Command{{Type: "command", Command: "guild quest brief --auto"}},
		}},
	}
	out, _ := mustMerge(t, raw, desired)

	var doc struct {
		Hooks map[string][]Group `json:"hooks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if n := len(doc.Hooks["SessionStart"]); n != 1 {
		t.Errorf("SessionStart has %d groups; want 1 (stale guild group dropped)", n)
	}
	stop := doc.Hooks["Stop"]
	if len(stop) != 1 {
		t.Fatalf("Stop has %d groups; want 1 (guild gone, foreign kept)", len(stop))
	}
	if stop[0].Hooks[0].Command != "other-tool cleanup" {
		t.Errorf("Stop survivor = %q; want the foreign command", stop[0].Hooks[0].Command)
	}
}

// TestMerge_CorruptDocumentIsError: never clobber a file we cannot parse.
func TestMerge_CorruptDocumentIsError(t *testing.T) {
	if _, _, err := MergeSettingsDoc([]byte("{broken"), DefaultBase(), "hooks"); err == nil {
		t.Error("MergeSettingsDoc on corrupt input returned nil error")
	}
}

// --- scan ----------------------------------------------------------------

// TestScanSettingsDoc_ReportsGuildAndForeign: the flattened view tags
// ownership per group and includes non-guild hooks.
func TestScanSettingsDoc_ReportsGuildAndForeign(t *testing.T) {
	raw := []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|resume|clear|compact", "hooks": [{"type": "command", "command": "guild quest brief --auto"}]},
      {"matcher": "startup", "hooks": [{"type": "command", "command": "other-tool prime"}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "guild lore appraise --inject --from-stdin-json"}]}
    ]
  }
}`)
	hs, err := ScanSettingsDoc(raw, "hooks")
	if err != nil {
		t.Fatalf("ScanSettingsDoc: %v", err)
	}
	if len(hs) != 3 {
		t.Fatalf("scanned %d hooks; want 3", len(hs))
	}

	byCommand := map[string]Hook{}
	for _, h := range hs {
		byCommand[h.Command] = h
	}
	if h := byCommand["guild quest brief --auto"]; !h.GuildOwned || h.Event != "SessionStart" {
		t.Errorf("guild brief hook misreported: %+v", h)
	}
	if h := byCommand["other-tool prime"]; h.GuildOwned {
		t.Errorf("foreign hook reported guild-owned: %+v", h)
	}
	if h := byCommand["guild lore appraise --inject --from-stdin-json"]; h.Matcher != "" {
		t.Errorf("UserPromptSubmit hook has matcher %q; want none", h.Matcher)
	}
}

func TestScanSettingsDoc_EmptyAndMissingSection(t *testing.T) {
	if hs, err := ScanSettingsDoc(nil, "hooks"); err != nil || len(hs) != 0 {
		t.Errorf("empty doc: hooks=%v err=%v; want empty, nil", hs, err)
	}
	if hs, err := ScanSettingsDoc([]byte(`{"model": "opus"}`), "hooks"); err != nil || len(hs) != 0 {
		t.Errorf("doc without hooks section: hooks=%v err=%v; want empty, nil", hs, err)
	}
}

// --- helpers -------------------------------------------------------------

func TestApplySubstitution(t *testing.T) {
	base := Config{"SessionStart": {{Matcher: "startup", Hooks: []Command{{Type: "command", Command: "guild quest brief PLACEHOLDER"}}}}}

	got := ApplySubstitution(base, func(cmd string) string {
		return "guild quest brief --auto"
	})
	if got["SessionStart"][0].Hooks[0].Command != "guild quest brief --auto" {
		t.Errorf("substitution not applied: %+v", got)
	}
	// Original untouched (deep copy).
	if base["SessionStart"][0].Hooks[0].Command != "guild quest brief PLACEHOLDER" {
		t.Errorf("ApplySubstitution mutated its input: %+v", base)
	}
	// Nil sub copies verbatim.
	id := ApplySubstitution(base, nil)
	if id["SessionStart"][0].Hooks[0].Command != "guild quest brief PLACEHOLDER" {
		t.Errorf("nil substitution altered command: %+v", id)
	}
}

func TestFlatten_SortedAndOwned(t *testing.T) {
	cfg := Config{
		"UserPromptSubmit": {{Hooks: []Command{{Type: "command", Command: "guild lore appraise --inject --from-stdin-json"}}}},
		"SessionStart":     {{Matcher: "startup", Hooks: []Command{{Type: "command", Command: "other-tool prime"}}}},
	}
	hs := Flatten(cfg)
	if len(hs) != 2 {
		t.Fatalf("flattened %d hooks; want 2", len(hs))
	}
	if hs[0].Event != "SessionStart" || hs[1].Event != "UserPromptSubmit" {
		t.Errorf("events not sorted: %+v", hs)
	}
	if hs[0].GuildOwned {
		t.Error("foreign hook flagged guild-owned")
	}
	if !hs[1].GuildOwned {
		t.Error("guild hook not flagged guild-owned")
	}
}
