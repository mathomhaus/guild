package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// This file is the transport-agnostic JSON execution surface of the
// command registry. It exists so a terminal CLI verb can run its
// Handler inside the guild daemon (single writer) while RENDERING stays
// in the client process and byte-identical:
//
//	client: build I from cobra flags → json.Marshal(I) → daemon
//	daemon: json.Unmarshal(I) → Handler(ctx, daemonDeps, in) → json.Marshal(O)
//	client: json.Unmarshal(O) → CLIFormat / CLIWarnings / --json / agent envelope
//
// Routing via the MCP tool surface would be wrong here: MCPFormat
// renders differently from CLIFormat, and the CLI must keep its exact
// output bytes. The domain result O is the unit shipped over the wire,
// never rendered text.

// RemoteExecRequest is the unit Deps.ExecRemote ships to the daemon:
// one verb name, its JSON-encoded input struct, and the render context
// the daemon needs for bespoke error narration (CLIErrorFormat runs
// daemon-side because typed handler errors do not survive JSON).
type RemoteExecRequest struct {
	// Tool is the wire name, e.g. "quest_post".
	Tool string
	// Args is the json.Marshal of the verb's input struct I.
	Args json.RawMessage
	// NoEmoji mirrors the client's --no-emoji / GUILD_NO_EMOJI setting
	// so daemon-side error narration uses the same sink the client
	// would have.
	NoEmoji bool
}

// RemoteHandlerError is a verb Handler failure that happened inside the
// daemon. It is FINAL: the verb ran (side effects may have been
// applied), so the cobra adapter must never re-run the Handler locally.
// Error() carries the handler's exact error string and AgentHint() the
// recovered hint, so human, --json, and agent-envelope renderings stay
// byte-identical with a local failure. Narration carries the verb's
// CLIErrorFormat output, rendered daemon-side against the typed error.
type RemoteHandlerError struct {
	Message     string
	Hint        string
	Narration   string
	NarrationOK bool
}

func (e *RemoteHandlerError) Error() string { return e.Message }

// AgentHint surfaces the daemon-side recovered hint in the agent-mode
// envelope, mirroring hintedError.AgentHint for local execution.
func (e *RemoteHandlerError) AgentHint() string { return e.Hint }

// InputRestorer is implemented (with a pointer receiver) by output
// types whose CLI rendering reads input-derived fields excluded from
// the JSON wire shape (tagged `json:"-"`). After a daemon round trip
// the cobra adapter re-attaches the local input so rendering stays
// byte-identical with in-process execution. quest.ListOutput is the
// reference implementation.
type InputRestorer[I any] interface {
	RestoreInput(in I)
}

// execExemptions lists wire names that must always execute locally,
// keyed to the documented reason. The list exists for verbs whose
// remote execution cannot reproduce a local run byte-for-byte: either
// because the domain output cannot round-trip JSON losslessly, or
// because the Handler reads caller-process state the daemon does not
// have. Target size: zero; every entry needs a PR-documented reason.
var execExemptions = map[string]string{
	// quest_orders resolves the default agent identity from the CALLER's
	// environment (PM_OWNER → GUILD_AGENT → USER) inside quest.Orders
	// when --agent is not given. The daemon's environment is not the
	// caller's, so a routed run could query (and report) a different
	// identity. Read-only verb: exempting it costs nothing toward the
	// single-writer goal.
	"quest_orders": "agent identity defaults resolve from the caller's environment (PM_OWNER/GUILD_AGENT/USER), which the daemon does not share",
}

// ExecExemptionReason reports whether the named verb is exempt from
// daemon routing and why. Exposed so the daemon-side registry refuses
// exempt verbs even if a skewed client asks for one.
func ExecExemptionReason(name string) (string, bool) {
	reason, ok := execExemptions[name]
	return reason, ok
}

// dispatchHandler runs the verb's business logic, preferring the
// daemon when Deps carries an ExecRemote transport. Fallback contract
// (ADR-005: correctness never depends on the daemon):
//
//   - no transport, exempt verb, or input marshal failure → local Handler
//   - transport error (daemon down, conn drop, dispatch refusal) →
//     local Handler; the command still succeeds
//   - *RemoteHandlerError → FINAL: the Handler already ran inside the
//     daemon; never re-run locally (double-applied writes)
//   - undecodable remote result → error (same double-write rationale);
//     unreachable in practice because routing requires an exact
//     version match
func (c *Command[I, O]) dispatchHandler(ctx context.Context, d Deps, in I, noEmoji bool) (O, error) {
	var zero O
	if d.ExecRemote == nil {
		return c.Handler(ctx, d, in)
	}
	if _, exempt := ExecExemptionReason(c.Name); exempt {
		return c.Handler(ctx, d, in)
	}
	args, err := json.Marshal(in)
	if err != nil {
		return c.Handler(ctx, d, in)
	}
	raw, rerr := d.ExecRemote(ctx, RemoteExecRequest{Tool: c.Name, Args: args, NoEmoji: noEmoji})
	if rerr != nil {
		var remote *RemoteHandlerError
		if errors.As(rerr, &remote) {
			return zero, rerr
		}
		return c.Handler(ctx, d, in)
	}
	var out O
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return zero, fmt.Errorf("%s: decode daemon result: %w", c.Name, uerr)
	}
	if r, ok := any(&out).(InputRestorer[I]); ok {
		r.RestoreInput(in)
	}
	return out, nil
}

// ExecHandlerError is the structured shape of a Handler failure crossing
// the JSON-exec boundary, produced daemon-side by ExecRegistry.Exec.
// The transport layer (internal/daemon) carries a wire twin; the CLI
// reconstructs a *RemoteHandlerError from it.
type ExecHandlerError struct {
	// Message is the handler error's exact Error() string.
	Message string
	// Hint is the agent-mode recovery hint recovered via
	// AgentHintFromError, empty when none was attached.
	Hint string
	// Narration is the verb's CLIErrorFormat rendering of the typed
	// error, valid only when NarrationOK is true.
	Narration   string
	NarrationOK bool
}

// DepsBuilder constructs the Deps bundle for one JSON-exec invocation.
// cwd is the CLIENT's working directory: project resolution must anchor
// there, not on the daemon's own cwd.
type DepsBuilder func(ctx context.Context, cwd string) Deps

// ExecRegistry is the daemon-side dispatch table: wire name → typed
// JSON-exec adapter. Built once per daemon process from the same
// Command specs the terminal CLI binds, so the two sides can never
// disagree about a verb's input or output shape.
type ExecRegistry struct {
	entries map[string]execEntry
}

type execEntry struct {
	deps DepsBuilder
	run  func(ctx context.Context, d Deps, noEmoji bool, args json.RawMessage) (json.RawMessage, *ExecHandlerError, error)
}

// NewExecRegistry returns an empty dispatch table.
func NewExecRegistry() *ExecRegistry {
	return &ExecRegistry{entries: map[string]execEntry{}}
}

// RegisterExec adds c to the table with its Deps builder. Exempt verbs
// are silently skipped: the client never routes them, and skipping here
// guarantees a misbehaving client cannot make the daemon run one either.
func RegisterExec[I, O any](r *ExecRegistry, c *Command[I, O], deps DepsBuilder) {
	if _, exempt := ExecExemptionReason(c.Name); exempt {
		return
	}
	r.entries[c.Name] = execEntry{deps: deps, run: execAdapter(c)}
}

// BindExec registers this command on r with its Deps builder, the
// type-erased equivalent of RegisterExec. It exists so a caller holding a
// []Registrant (the ADR-006 module loop) can build the daemon-side exec
// dispatch table from a module's Commands() without recovering each
// command's concrete generic type. Behavior is identical to RegisterExec,
// including the exec-exemption skip, because both route through the same
// execAdapter closure.
func (c *Command[I, O]) BindExec(r *ExecRegistry, deps DepsBuilder) {
	RegisterExec(r, c, deps)
}

// Names returns the registered wire names, sorted. Test/diagnostic use.
func (r *ExecRegistry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Exec runs one verb from JSON args to a JSON result. Return contract:
//
//   - (result, nil, nil): Handler succeeded; result is json.Marshal(O).
//   - (nil, handlerErr, nil): Handler ran and failed. FINAL for the
//     client; no local re-run.
//   - (nil, nil, err): dispatch failure (unknown verb, undecodable
//     args). The Handler did NOT run; the client falls back to local
//     execution.
func (r *ExecRegistry) Exec(ctx context.Context, tool, cwd string, noEmoji bool, args json.RawMessage) (json.RawMessage, *ExecHandlerError, error) {
	e, ok := r.entries[tool]
	if !ok {
		return nil, nil, fmt.Errorf("guild_exec: unknown verb %q", tool)
	}
	return e.run(ctx, e.deps(ctx, cwd), noEmoji, args)
}

// execAdapter closes over one typed Command, erasing I and O behind the
// JSON boundary. The Handler invoked here is the same function BindCobra
// and BindMCP call; only the encode/decode shell differs.
func execAdapter[I, O any](c *Command[I, O]) func(ctx context.Context, d Deps, noEmoji bool, args json.RawMessage) (json.RawMessage, *ExecHandlerError, error) {
	return func(ctx context.Context, d Deps, noEmoji bool, args json.RawMessage) (json.RawMessage, *ExecHandlerError, error) {
		var in I
		if len(args) > 0 && !bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
			if err := json.Unmarshal(args, &in); err != nil {
				// The Handler has not run: report a dispatch failure so the
				// client safely retries locally.
				return nil, nil, fmt.Errorf("guild_exec %s: decode args: %w", c.Name, err)
			}
		}
		out, herr := c.Handler(ctx, d, in)
		if herr != nil {
			he := &ExecHandlerError{
				Message: herr.Error(),
				Hint:    AgentHintFromError(herr),
			}
			if c.CLIErrorFormat != nil {
				if msg, ok := c.CLIErrorFormat(CLISink{NoEmoji: noEmoji}, herr); ok {
					he.Narration, he.NarrationOK = msg, true
				}
			}
			return nil, he, nil
		}
		buf, merr := json.Marshal(out)
		if merr != nil {
			// Effectively unreachable: every registered O is a plain data
			// struct. Reported as a handler-style failure (NOT a dispatch
			// failure) because the Handler DID run, and a local re-run could
			// double-apply writes.
			return nil, &ExecHandlerError{Message: fmt.Sprintf("%s: encode result: %v", c.Name, merr)}, nil
		}
		return buf, nil, nil
	}
}
