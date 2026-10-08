package cli

// setupModuleVerbs binds the module-registry verbs onto questCmd / loreCmd
// at an EXPLICIT, ordered point in the package's startup wiring (ADR-006
// Phase 3 cleanup), instead of relying on this file's name sorting between
// quest.go and root.go in Go's lexical init order. root.go's init calls it
// directly, immediately BEFORE attaching the command groups to rootCmd, so
// the load-bearing bind window the pre-cutover hand lists relied on is
// preserved by call order rather than by filename:
//
//   - questCmd's persistent --project / -p flag and loreCmd's bespoke
//     (non-registry) subcommands are already set up: quest.go and the
//     lore_*.go init()s run before root.go's (and so before this call),
//     because root.go sorts last lexically. Each bound quest verb thus
//     inherits -p and command.BindCobra skips re-declaring a colliding
//     local one.
//   - questCmd and loreCmd are NOT yet attached to rootCmd at the call
//     site (AddCommand runs right after this), so rootCmd's persistent
//     flags (--agent Bool, --no-emoji, --verbose) are not yet inherited
//     into the verb flagsets. This matters: quest journal/orders/campfire/
//     summon declare a LOCAL string --agent (agent identity); binding them
//     before the root Bool --agent is inherited keeps that registration
//     clean and avoids the pflag "--agent flag redefined" panic.
//
// Pinning the timing to an explicit call (not the filename
// "register_modules.go" sorting after quest.go but before root.go) means a
// renamed or newly added cli source file can no longer silently shift the
// bind out of its window. bindModuleVerbs (modules.go) does the actual
// per-module work; this seam is solely about WHEN it runs.
func setupModuleVerbs() {
	bindModuleVerbs()
}
