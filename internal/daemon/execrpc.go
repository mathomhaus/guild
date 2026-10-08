package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// This file is the JSON-exec RPC: the socket protocol a terminal CLI
// uses to run ONE registry verb inside the daemon process (single
// writer) while keeping all rendering client-side. Wire shape, one
// exchange per connection:
//
//	client → {"guild_exec":{"tool":"quest_post","version":"v0.3.2","cwd":"/p","pid":4242}}\n
//	client → <args JSON, the verb's input struct>\n
//	daemon → {"ok":true,"result":{...}}\n            (handler succeeded)
//	       | {"ok":false,"handler_error":{...}}\n    (handler ran and failed; final)
//	       | {"ok":false,"error":"..."}\n            (dispatch refused; client runs locally)
//	conn closes.
//
// The three response arms matter: a handler_error means side effects
// may exist and the client must NOT re-run the verb, while a bare
// error means the handler never ran and local fallback is safe.

// execDialTimeout bounds the unix-socket dial. Local dials complete in
// microseconds; a daemon that cannot accept within a second is treated
// as down and the client falls back to local execution.
const execDialTimeout = time.Second

// execWriteTimeout bounds writing the preamble + args (client) and the
// response line (daemon). Both are small local writes.
const execWriteTimeout = 10 * time.Second

// execArgsMaxBytes caps the args line the daemon reads. Verb inputs are
// flag-sized strings (the largest realistic one is a quest spec text);
// 4 MiB is far above anything legitimate while still bounding a
// misbehaving peer.
const execArgsMaxBytes = 4 << 20

// execResponseMaxBytes caps the response line the client reads. Domain
// results can carry whole quest/lore listings, so the cap is generous.
const execResponseMaxBytes = 64 << 20

// ExecPreamble is the identity line a CLI client sends to run one verb
// in the daemon, wrapped on the wire as {"guild_exec":{...}}.
type ExecPreamble struct {
	// Tool is the registry wire name, e.g. "quest_post".
	Tool string `json:"tool"`
	// Version is the client binary's ldflags-stamped version. The
	// daemon refuses on any difference (exact string equality, the
	// version-skew rule from discovery.go) so a skewed client falls
	// back to local execution even when its probe raced a daemon swap.
	Version string `json:"version"`
	// CWD is the client process's working directory; project resolution
	// anchors here exactly as a local run's os.Getwd would.
	CWD string `json:"cwd"`
	// PID is the client's process id, recorded in the daemon log.
	PID int `json:"pid"`
	// NoEmoji mirrors the client's --no-emoji / GUILD_NO_EMOJI setting
	// for daemon-side error narration.
	NoEmoji bool `json:"no_emoji,omitempty"`
}

// ExecHandlerError is the wire twin of command.ExecHandlerError: a verb
// Handler failure that happened inside the daemon. Final for the
// client: the verb ran, so a local re-run could double-apply writes.
type ExecHandlerError struct {
	Message     string `json:"message"`
	Hint        string `json:"hint,omitempty"`
	Narration   string `json:"narration,omitempty"`
	NarrationOK bool   `json:"narration_ok,omitempty"`
}

// ExecRequest is what the daemon hands its ExecHandler after parsing
// one exec connection: preamble identity plus the raw args line.
type ExecRequest struct {
	Tool    string
	CWD     string
	NoEmoji bool
	Args    json.RawMessage
}

// ExecHandler executes one registry verb on the daemon's side of the
// JSON-exec RPC. Return contract mirrors command.ExecRegistry.Exec:
// (result, nil, nil) on success, (nil, handlerErr, nil) when the verb
// ran and failed, (nil, nil, err) when dispatch was refused before the
// verb ran. The production handler is built by internal/cli's
// NewDaemonExecHandler; the indirection keeps this package a leaf.
type ExecHandler func(ctx context.Context, req ExecRequest) (json.RawMessage, *ExecHandlerError, error)

// execResponse is the single line the daemon writes back.
type execResponse struct {
	OK           bool              `json:"ok"`
	Result       json.RawMessage   `json:"result,omitempty"`
	HandlerError *ExecHandlerError `json:"handler_error,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// serveExec answers one exec connection end to end: version gate, args
// line, dispatch, one response line. The caller (serveConn) closes the
// connection afterwards.
func (s *Server) serveExec(ctx context.Context, br *bufio.Reader, conn net.Conn, pre ExecPreamble) {
	// Activity for the idle scheduler: a routed CLI verb is a waking
	// operator just like an MCP session. Touch on entry (preempting any
	// in-flight dream pass before the verb runs) and again on return
	// (below) so a long verb like an embed rebuild brackets the idle
	// clock. Touch is a no-op when no scheduler is wired (Phase 1).
	s.touch()
	defer s.touch()

	if pre.Version != s.cfg.Version {
		s.log.Warn("daemon: exec refused on version skew",
			"tool", pre.Tool, "client_version", pre.Version, "daemon_version", s.cfg.Version, "client_pid", pre.PID)
		s.writeExecResponse(conn, execResponse{
			Error: fmt.Sprintf("daemon: version mismatch: daemon %s, client %s", s.cfg.Version, pre.Version),
		})
		return
	}
	if s.cfg.Exec == nil {
		s.writeExecResponse(conn, execResponse{Error: "daemon: exec is not supported by this daemon"})
		return
	}

	// The args line follows the preamble immediately; a fresh deadline
	// keeps a wedged client from pinning this goroutine.
	_ = conn.SetReadDeadline(time.Now().Add(preambleTimeout))
	args, err := readLineCapped(br, execArgsMaxBytes)
	if err != nil {
		s.log.Warn("daemon: exec dropping connection with bad args line", "tool", pre.Tool, "err", err)
		s.writeExecResponse(conn, execResponse{Error: fmt.Sprintf("daemon: read exec args: %v", err)})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	result, handlerErr, err := s.cfg.Exec(ctx, ExecRequest{
		Tool:    pre.Tool,
		CWD:     pre.CWD,
		NoEmoji: pre.NoEmoji,
		Args:    json.RawMessage(args),
	})

	outcome := "ok"
	switch {
	case err != nil:
		outcome = "dispatch_error"
	case handlerErr != nil:
		outcome = "handler_error"
	}
	// This log line is the daemon-side execution record: integration
	// tests assert on it to prove the Handler ran in THIS process.
	s.log.Info("daemon: exec",
		"tool", pre.Tool,
		"cwd", pre.CWD,
		"client_pid", pre.PID,
		"outcome", outcome,
	)

	switch {
	case err != nil:
		s.writeExecResponse(conn, execResponse{Error: err.Error()})
	case handlerErr != nil:
		s.writeExecResponse(conn, execResponse{HandlerError: handlerErr})
	default:
		s.writeExecResponse(conn, execResponse{OK: true, Result: result})
	}
}

// writeExecResponse marshals and writes the one response line.
func (s *Server) writeExecResponse(conn net.Conn, resp execResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.log.Warn("daemon: marshal exec response", "err", err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(execWriteTimeout))
	if _, err := conn.Write(append(data, '\n')); err != nil {
		s.log.Warn("daemon: write exec response", "err", err)
	}
}

// readLineCapped reads one '\n'-terminated line from br, enforcing max
// bytes. Same ReadSlice loop as readPreambleLine but with a
// caller-chosen cap (exec args and responses are legitimately larger
// than a preamble).
func readLineCapped(br *bufio.Reader, maxBytes int) ([]byte, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		line = append(line, frag...)
		if len(line) > maxBytes {
			return nil, fmt.Errorf("daemon: line exceeds %d bytes", maxBytes)
		}
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, fmt.Errorf("daemon: read line: %w", err)
	}
}

// ExecCall is the client half: dial the daemon socket, perform one exec
// exchange, return the verb's JSON result. Return contract mirrors
// ExecHandler: a non-nil *ExecHandlerError is FINAL (the verb ran in
// the daemon); a plain error is a transport/dispatch failure after
// which the caller may safely execute locally.
func ExecCall(ctx context.Context, socketPath string, pre ExecPreamble, args json.RawMessage) (json.RawMessage, *ExecHandlerError, error) {
	dialer := net.Dialer{Timeout: execDialTimeout}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: exec dial %s: %w", socketPath, err)
	}
	defer func() { _ = conn.Close() }()

	// Tie the blocking reads/writes below to ctx cancellation (Ctrl-C):
	// closing the conn is what unblocks them.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	preLine, err := json.Marshal(struct {
		Exec ExecPreamble `json:"guild_exec"`
	}{Exec: pre})
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: marshal exec preamble: %w", err)
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	// Preamble and args in one write: json.Marshal output never contains
	// raw newlines, so each is exactly one ndjson line.
	payload := make([]byte, 0, len(preLine)+len(args)+2)
	payload = append(payload, preLine...)
	payload = append(payload, '\n')
	payload = append(payload, args...)
	payload = append(payload, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(execWriteTimeout))
	if _, err := conn.Write(payload); err != nil {
		return nil, nil, fmt.Errorf("daemon: write exec request: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	// No fixed read deadline: verbs may legitimately run long (embed
	// rebuilds). ctx cancellation and the daemon hanging up both unblock
	// the read via the conn close above.
	line, err := readLineCapped(bufio.NewReader(conn), execResponseMaxBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: read exec response: %w", err)
	}
	resp := execResponse{}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, nil, fmt.Errorf("daemon: parse exec response: %w", err)
	}
	switch {
	case resp.OK:
		return resp.Result, nil, nil
	case resp.HandlerError != nil:
		return nil, resp.HandlerError, nil
	case resp.Error != "":
		return nil, nil, errors.New(resp.Error)
	default:
		return nil, nil, errors.New("daemon: exec response carries neither result nor error")
	}
}
