package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// execTimeout bounds every one-shot `docker ...` invocation. Generous
// because the first guild call inside a fresh container extracts and
// loads the embedded ONNX runtime.
const execTimeout = 2 * time.Minute

// projectDir is where every scenario registers its project inside the
// container. Fixed (not random) so transcripts are byte-stable.
const projectDir = "/home/guild/e2eproj"

// container is one isolated guild environment: a long-lived docker
// container whose only job is to host guild state (/home/guild/.guild)
// and guild processes for a single scenario. State never leaves the
// container; it dies with it.
type container struct {
	name  string
	image string
	// env are extra KEY=VALUE pairs injected as `-e` flags into every
	// docker exec for this container (CLI calls, MCP sessions, daemon
	// start). Empty for the golden scenarios, which must stay
	// byte-identical; the module-toggle proof sets GUILD_MODULE_LORE=0
	// here so every surface in that container sees the disable.
	env []string
}

// extraEnvArgs returns the `-e KEY=VALUE` argv fragment for the
// container's configured env, in declaration order.
func (c *container) extraEnvArgs() []string {
	args := make([]string, 0, len(c.env)*2)
	for _, kv := range c.env {
		args = append(args, "-e", kv)
	}
	return args
}

// startContainer launches a fresh scenario container and registers
// cleanup on t. The container:
//   - runs --network none: guild needs no network at runtime (embedded
//     model assets, update check disabled), and this proves it;
//   - idles on /bin/sleep so guild processes are started per-step via
//     docker exec, all sharing the same container-local state.
func startContainer(ctx context.Context, t *testing.T) *container {
	t.Helper()

	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	c := &container{
		name:  "guild-e2e-" + hex.EncodeToString(rnd[:]),
		image: suite.image,
	}

	out, err := dockerCombined(ctx,
		"run", "-d", "--rm",
		"--name", c.name,
		"--network", "none",
		"--label", "guild-e2e=1",
		"--entrypoint", "/bin/sleep",
		c.image, "infinity",
	)
	if err != nil {
		t.Fatalf("start container %s: %v\n%s", c.name, err, out)
	}
	//nolint:contextcheck // cleanup deliberately detaches from the scenario ctx, which is typically already cancelled when t.Cleanup runs
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := dockerCombined(cctx, "rm", "-f", c.name); err != nil {
			t.Logf("cleanup container %s: %v\n%s", c.name, err, out)
		}
	})
	return c
}

// guild runs a one-shot guild CLI command inside the container with the
// scenario project dir as the working directory and returns combined
// output. Fails t on a non-zero exit.
func (c *container) guild(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	argv := []string{
		"exec",
		"-w", projectDir,
		"-e", "GUILD_NO_UPDATE_CHECK=1",
	}
	argv = append(argv, c.extraEnvArgs()...)
	argv = append(argv, c.name, "guild")
	argv = append(argv, args...)
	out, err := dockerCombined(ctx, argv...)
	if err != nil {
		t.Fatalf("guild %v in %s: %v\n%s", args, c.name, err, out)
	}
	return out
}

// initProject creates the scenario project directory and runs
// `guild init --yes` in it, registering the project (basename of
// projectDir) in the container-local lore.db + quest.db. Returns the
// init output for transcript recording.
//
// In daemon mode it then starts the in-container daemon (after init, so
// the DB creation itself still runs on the plain in-process path) and
// waits for readiness. Every MCP session opened afterward routes through
// the daemon via the shim, and the same golden transcripts must still
// hold: the ADR-005 Phase 1 byte-identical invariant.
func (c *container) initProject(ctx context.Context, t *testing.T) string {
	t.Helper()
	if out, err := dockerCombined(ctx, "exec", c.name, "mkdir", "-p", projectDir); err != nil {
		t.Fatalf("mkdir %s in %s: %v\n%s", projectDir, c.name, err, out)
	}
	out := c.guild(ctx, t, "init", "--yes")
	if suite.mode == modeDaemon {
		c.startDaemon(ctx, t)
	}
	return out
}

// startDaemon launches the in-container daemon for daemon-mode runs and
// blocks until `guild daemon status` reports it running. The container's
// own teardown (docker rm -f) reaps the daemon process, so no separate
// stop is needed; the wait keeps the first session from racing the
// daemon's socket publication.
func (c *container) startDaemon(ctx context.Context, t *testing.T) {
	t.Helper()
	_ = c.guild(ctx, t, "daemon", "start")

	deadline := time.Now().Add(30 * time.Second)
	for {
		statusArgs := []string{"exec", "-w", projectDir, "-e", "GUILD_NO_UPDATE_CHECK=1"}
		statusArgs = append(statusArgs, c.extraEnvArgs()...)
		statusArgs = append(statusArgs, c.name, "guild", "daemon", "status")
		out, err := dockerCombined(ctx, statusArgs...)
		if err == nil {
			return // exit 0 → running
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-container daemon did not become ready within 30s\nlast status:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dockerCombined runs `docker argv...` with a deadline and returns
// combined stdout+stderr.
func dockerCombined(ctx context.Context, argv ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	//nolint:gosec // argv is harness-controlled (image name, container name, fixed paths)
	cmd := exec.CommandContext(ctx, "docker", argv...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("%w (after %s timeout)", err, execTimeout)
	}
	return buf.String(), err
}
