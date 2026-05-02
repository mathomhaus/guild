package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mathomhaus/guild/internal/cli"
	"github.com/mathomhaus/guild/internal/mcp"
)

// wireMCPServe wires the `guild mcp serve` cobra RunE onto
// internal/cli's mcpServeCmd placeholder. Factored into its own file
// (and its own init) per QUEST-12 scope boundary: root.go must stay
// MCP-agnostic, so the wiring lives here where the mcp + cli packages
// are already linked by the main binary. The cli package exposes
// SetMCPServeRunE as the single attachment seam — see
// internal/cli/root.go.
func wireMCPServe() {
	cli.SetMCPServeRunE(runMCPServe)
}

// init runs at binary startup, before cobra.Execute. Wiring via init
// means the cmd/guild build is the only place that assembles the MCP
// handler; tests that import internal/cli directly (see root_test.go)
// keep seeing the [errNotImplemented] placeholder and don't pull in
// the SDK.
func init() {
	wireMCPServe()
	// Wire the ldflags-stamped version into the MCP layer so
	// guild_session_start can emit an upgrade nudge when appropriate.
	mcp.SetBinaryVersion(version)
}

// runMCPServe is the cobra RunE for `guild mcp serve`. Uses a
// [signal.NotifyContext] on SIGINT + SIGTERM so Ctrl-C and `kill <pid>`
// cleanly cancel the server ctx and let [mcp.Serve] tear down the stdio
// transport.
//
// The parent ctx (context.Background) is used rather than
// cmd.Context(): cobra's default context is uncancelled at this point
// and we want ONLY signal-driven cancellation for the server loop.
// Tests that exercise mcp.Serve directly build their own ctx with
// their own cancel function.
func runMCPServe(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// stop is documented to be idempotent; deferred release ensures
	// the signal handler is unregistered even if Serve returns early
	// with a protocol error (and not via ctx-cancel).
	defer stop()

	if err := mcp.Serve(ctx); err != nil {
		// Surface ONLY the error (no stderr banner); cobra's
		// SilenceUsage handles the usage print. The error message
		// travels up to main.go which prints "error: <msg>" once.
		return fmt.Errorf("mcp serve: %w", err)
	}
	return nil
}
