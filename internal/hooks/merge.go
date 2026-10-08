package hooks

// Ownership-aware merge of the guild base config into a harness
// settings document.
//
// Ownership semantics (ADR-004 Refinement log entry 3, superseding the
// original per-matcher merge): a hook group is guild-owned iff every
// command in group.hooks[] matches ^guild( |$). Sync replaces
// guild-owned groups in place and appends missing ones; foreign groups
// and mixed groups (guild + foreign commands in one hooks array) are
// preserved byte-for-byte, unknown JSON fields included. Mixed groups
// counting as foreign is deliberate conservatism: never partially
// rewrite a group the user has edited.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// guildOwnedRe identifies a guild command: the literal token "guild"
// followed by an argument separator or end of string. "guildctl foo"
// does not match; "guild quest brief --auto" does.
var guildOwnedRe = regexp.MustCompile(`^guild( |$)`)

// CommandIsGuild reports whether a single command string is a guild
// command per the ownership rule above.
func CommandIsGuild(cmd string) bool {
	return guildOwnedRe.MatchString(cmd)
}

// GroupIsGuildOwned reports whether every command in the group is a
// guild command. Empty groups are NOT guild-owned: there is no evidence
// they are ours, so they are left alone.
func GroupIsGuildOwned(g Group) bool {
	if len(g.Hooks) == 0 {
		return false
	}
	for _, h := range g.Hooks {
		if !CommandIsGuild(h.Command) {
			return false
		}
	}
	return true
}

// Flatten renders cfg as the flattened Hook list, sorted by event name
// with group and command order preserved within each event. GuildOwned
// is computed per group. Used to compare desired state against an
// adapter's Scan output.
func Flatten(cfg Config) []Hook {
	events := make([]string, 0, len(cfg))
	for ev := range cfg {
		events = append(events, ev)
	}
	sort.Strings(events)

	var out []Hook
	for _, ev := range events {
		for _, g := range cfg[ev] {
			owned := GroupIsGuildOwned(g)
			for _, h := range g.Hooks {
				out = append(out, Hook{
					Event:      ev,
					Matcher:    g.Matcher,
					Command:    h.Command,
					GuildOwned: owned,
				})
			}
		}
	}
	return out
}

// ApplySubstitution returns a deep copy of cfg with every command
// string mapped through sub (an adapter's Substitute). A nil sub copies
// verbatim.
func ApplySubstitution(cfg Config, sub func(string) string) Config {
	out := make(Config, len(cfg))
	for ev, groups := range cfg {
		gs := make([]Group, len(groups))
		for i, g := range groups {
			hs := make([]Command, len(g.Hooks))
			for j, h := range g.Hooks {
				if sub != nil {
					h.Command = sub(h.Command)
				}
				hs[j] = h
			}
			gs[i] = Group{Matcher: g.Matcher, Hooks: hs}
		}
		out[ev] = gs
	}
	return out
}

// rawGroup pairs a group's raw JSON (preserved verbatim for foreign
// groups) with the parsed view used for ownership classification.
type rawGroup struct {
	raw    json.RawMessage
	parsed Group
}

// MergeSettingsDoc merges desired (the guild-owned intent, already
// substituted by the adapter) into the raw JSON settings document. Hook
// events live under the top-level hooksKey (e.g. "hooks" for Claude
// Code shaped settings files).
//
// Rules:
//   - foreign and mixed groups keep their original raw bytes, unknown
//     fields included, in their original positions;
//   - guild-owned groups are replaced positionally by the desired
//     groups for that event; extra guild-owned groups (stale state
//     from an older guild) are dropped; desired groups beyond the
//     existing guild-owned count are appended at the end;
//   - guild-owned groups under events absent from desired are removed
//     (sync regenerates the complete guild-owned state from base);
//   - every other top-level field of the document is preserved
//     verbatim.
//
// raw may be nil/empty (no settings file yet). The returned bytes are
// the canonical two-space-indented rendering; changed reports whether
// they differ from raw, so callers can skip the write on a no-op sync.
func MergeSettingsDoc(raw []byte, desired Config, hooksKey string) (out []byte, changed bool, err error) {
	if hooksKey == "" {
		return nil, false, fmt.Errorf("hooks: merge: empty hooksKey")
	}

	doc := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, false, fmt.Errorf("hooks: parse settings document: %w", err)
		}
	}

	events := map[string][]rawGroup{}
	if rawHooks, ok := doc[hooksKey]; ok {
		var byEvent map[string][]json.RawMessage
		if err := json.Unmarshal(rawHooks, &byEvent); err != nil {
			return nil, false, fmt.Errorf("hooks: parse %q section: %w", hooksKey, err)
		}
		for ev, rawGroups := range byEvent {
			gs := make([]rawGroup, len(rawGroups))
			for i, rg := range rawGroups {
				var g Group
				if err := json.Unmarshal(rg, &g); err != nil {
					return nil, false, fmt.Errorf("hooks: parse %q group in event %s: %w", hooksKey, ev, err)
				}
				gs[i] = rawGroup{raw: rg, parsed: g}
			}
			events[ev] = gs
		}
	}

	merged := map[string][]json.RawMessage{}

	// Union of event names, deterministic order (encoding/json sorts
	// map keys on marshal anyway; sorting here keeps the loop stable).
	names := map[string]struct{}{}
	for ev := range events {
		names[ev] = struct{}{}
	}
	for ev := range desired {
		names[ev] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for ev := range names {
		ordered = append(ordered, ev)
	}
	sort.Strings(ordered)

	for _, ev := range ordered {
		want := desired[ev]
		next := 0 // index of the next desired group to place
		var outGroups []json.RawMessage
		for _, g := range events[ev] {
			if !GroupIsGuildOwned(g.parsed) {
				outGroups = append(outGroups, g.raw)
				continue
			}
			if next < len(want) {
				enc, err := json.Marshal(want[next])
				if err != nil {
					return nil, false, fmt.Errorf("hooks: encode group for event %s: %w", ev, err)
				}
				outGroups = append(outGroups, enc)
				next++
			}
			// Guild-owned with no desired group left: drop (stale).
		}
		for ; next < len(want); next++ {
			enc, err := json.Marshal(want[next])
			if err != nil {
				return nil, false, fmt.Errorf("hooks: encode group for event %s: %w", ev, err)
			}
			outGroups = append(outGroups, enc)
		}
		if len(outGroups) > 0 {
			merged[ev] = outGroups
		}
	}

	if len(merged) > 0 {
		enc, err := json.Marshal(merged)
		if err != nil {
			return nil, false, fmt.Errorf("hooks: encode %q section: %w", hooksKey, err)
		}
		doc[hooksKey] = enc
	} else {
		delete(doc, hooksKey)
	}

	rendered, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("hooks: encode settings document: %w", err)
	}
	rendered = append(rendered, '\n')
	return rendered, !bytes.Equal(rendered, raw), nil
}

// ScanSettingsDoc extracts the flattened hook list (guild-owned AND
// foreign) from a raw settings document whose hook events live under
// hooksKey. Events come back sorted by name; group and command order is
// preserved within each event. An empty or hooks-less document yields
// an empty list.
func ScanSettingsDoc(raw []byte, hooksKey string) ([]Hook, error) {
	if hooksKey == "" {
		return nil, fmt.Errorf("hooks: scan: empty hooksKey")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("hooks: parse settings document: %w", err)
	}
	rawHooks, ok := doc[hooksKey]
	if !ok {
		return nil, nil
	}
	var cfg Config
	if err := json.Unmarshal(rawHooks, &cfg); err != nil {
		return nil, fmt.Errorf("hooks: parse %q section: %w", hooksKey, err)
	}
	return Flatten(cfg), nil
}
