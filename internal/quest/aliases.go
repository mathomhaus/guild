package quest

// toolAliases maps backward-compat MCP tool names to their canonical
// successors. Used by the hint engine to normalize trigger_tool matches
// so rules authored against the canonical name fire on alias calls too.
//
// Add entries here whenever a tool is renamed and the old name is kept
// as a backward-compat alias. The map is intentionally small — only
// renames that affect live hint rules need an entry.
var toolAliases = map[string]string{
	// QUEST-106 / LORE-80: quest_clear renamed to quest_fulfill.
	// quest_clear stays as a backward-compat MCP alias; hint rules use
	// quest_fulfill as the canonical trigger_tool.
	"quest_clear": "quest_fulfill",
}

// CanonicalToolName returns the canonical tool name for name. If name is
// a known backward-compat alias, the canonical successor is returned.
// Otherwise name is returned unchanged. Safe for concurrent use (reads a
// package-level map that is never mutated after init).
func CanonicalToolName(name string) string {
	if canonical, ok := toolAliases[name]; ok {
		return canonical
	}
	return name
}
