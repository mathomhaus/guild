package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"time"
)

// This file implements the stdio shim: the dumb-pipe mode `guild mcp
// serve` degrades into when the startup probe finds a live,
// version-matched daemon. The shim dials the daemon socket, sends the
// one-line identity preamble, and then forwards newline-delimited
// JSON-RPC frames verbatim in both directions. Frames are NEVER parsed
// or rewritten (raw-frame ruling); the only frame-awareness is
// buffering per line so each frame is delivered whole, and retaining
// the first two client frames (the MCP handshake) so a replacement
// endpoint can be spliced under the session if the daemon dies.
//
// Crash invariant: when the connection drops mid-session the shim
// re-dials once; if that fails it continues in-process via
// ShimConfig.Fallback. Either way the harness session keeps working
// and nothing durable is lost (all state lives in the DB). Frames that
// were in flight on the dead connection lose their responses; the
// harness's own request timeout handles those.

// ErrShimUnavailable is returned by RunShim when no daemon session was
// established and nothing was consumed from the stdio streams. The
// caller falls back to the in-process server exactly as if the startup
// probe had reported not_running; the no-daemon path stays
// byte-identical because the protocol streams are untouched.
var ErrShimUnavailable = errors.New("daemon: shim could not establish a daemon session")

// defaultShimDialTimeout bounds each shim dial: the initial connect
// right after a successful probe and the single re-dial after a lost
// connection. Local unix dials complete in microseconds; the bound
// only matters when the daemon is wedged mid-shutdown.
const defaultShimDialTimeout = 250 * time.Millisecond

// shimDrainTimeout bounds how long the shim waits for the daemon's
// trailing responses after the client closes stdin. The daemon answers
// the half-close promptly by ending the session; the timeout is purely
// defensive so a wedged daemon cannot pin an exiting harness.
const shimDrainTimeout = 5 * time.Second

// handshakeFrameCount is how many leading client frames the shim
// retains for replay: the initialize request (the client's first
// frame) and the initialized notification (its second). Identification
// is positional, not parsed: the MCP lifecycle requires a compliant
// client to open with exactly those two frames, and the raw-frame
// ruling forbids inspecting payloads. They are the only state needed
// to splice a fresh endpoint under an already-initialized session.
const handshakeFrameCount = 2

// ShimConfig configures one RunShim invocation.
type ShimConfig struct {
	// SocketPath is the live daemon's unix socket, taken from the
	// discovery record the startup probe returned.
	SocketPath string

	// Preamble identifies this shim to the daemon. PID and CWD are
	// required: the daemon validates the preamble and drops
	// connections that omit them (see readPreamble).
	Preamble ShimPreamble

	// Stdin and Stdout are the harness-facing protocol streams
	// (os.Stdin / os.Stdout in production). Required. Stdout carries
	// forwarded frames and nothing else; diagnostics go to Logger.
	Stdin  io.Reader
	Stdout io.Writer

	// DialTimeout bounds each dial. Non-positive means
	// defaultShimDialTimeout.
	DialTimeout time.Duration

	// Fallback continues the session in-process after the daemon was
	// lost and the one re-dial failed. It receives a reader that
	// replays the retained handshake (plus any undelivered frame)
	// ahead of the remaining Stdin, and a writer that suppresses the
	// duplicate initialize response when one is due. Nil means a lost
	// daemon ends the session with an error instead.
	Fallback func(ctx context.Context, r io.Reader, w io.Writer) error

	// Logger receives shim lifecycle warnings (connection lost,
	// re-dial, fallback). Nil discards them. Never wired to Stdout.
	Logger *slog.Logger
}

// RunShim pipes one MCP stdio session through the daemon socket until
// the client closes Stdin (returns nil), ctx is cancelled (returns
// nil, mirroring the in-process server's clean signal shutdown), or
// the session ends unrecoverably (returns the error).
//
// If the initial dial or preamble write fails, RunShim returns
// ErrShimUnavailable without having touched Stdin or Stdout, so the
// caller can serve in-process as if no daemon had been found.
func RunShim(ctx context.Context, cfg ShimConfig) error {
	if cfg.Stdin == nil || cfg.Stdout == nil {
		return errors.New("daemon: shim requires Stdin and Stdout")
	}
	if cfg.Preamble.PID <= 0 || cfg.Preamble.CWD == "" {
		return errors.New("daemon: shim preamble requires PID and CWD")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	s := &shim{cfg: cfg, log: log, stdin: bufio.NewReader(cfg.Stdin)}

	conn, err := s.dial()
	if err != nil {
		// The daemon vanished between the probe and this dial. Nothing
		// has been consumed from the protocol streams yet, so the
		// caller can still run the untouched in-process path.
		return fmt.Errorf("%w: %w", ErrShimUnavailable, err)
	}

	skipFirst := false
	for {
		out := s.session(ctx, conn, skipFirst)
		if out.done {
			return out.err
		}

		// The daemon side ended while the client is still attached:
		// re-dial once; if that fails, continue in-process.
		next, err := s.dial()
		if err != nil {
			s.log.Warn("guild daemon connection lost and re-dial failed; continuing in-process", "err", err)
			return s.fallback(ctx, out.pending)
		}
		if err := s.replay(next, out.pending); err != nil {
			_ = next.Close()
			s.log.Warn("guild daemon re-dial replay failed; continuing in-process", "err", err)
			return s.fallback(ctx, out.pending)
		}
		s.log.Warn("guild daemon connection lost; re-dialed and resumed")
		conn = next
		// The replayed initialize gets a fresh response from the new
		// endpoint; suppress it if the client already saw the original.
		skipFirst = s.respSeen && s.hs.count() > 0
	}
}

// shim is the state shared across the (possibly re-dialed) sessions of
// one RunShim call.
type shim struct {
	cfg ShimConfig
	log *slog.Logger

	// stdin buffers the client stream; it survives re-dials and is
	// handed to the in-process fallback so no client byte is lost.
	stdin *bufio.Reader

	// hs retains the first two client frames for handshake replay.
	hs handshakeLog

	// respSeen flips once any daemon frame reached Stdout. Until then
	// the client has not seen the initialize response, so a replayed
	// initialize's response must pass through rather than be
	// suppressed. Written by the conn pump, read by the controller
	// strictly after the pump's channel send (happens-before).
	respSeen bool
}

// dial connects to the daemon socket and writes the identity preamble.
// Returns a connection ready for raw frame traffic.
func (s *shim) dial() (net.Conn, error) {
	timeout := s.cfg.DialTimeout
	if timeout <= 0 {
		timeout = defaultShimDialTimeout
	}
	conn, err := net.DialTimeout("unix", s.cfg.SocketPath, timeout)
	if err != nil {
		return nil, err
	}
	pre, err := marshalShimPreamble(s.cfg.Preamble)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// The preamble is a few hundred bytes into an empty socket buffer;
	// the deadline only guards against a daemon wedged mid-shutdown.
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(pre); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("daemon: shim preamble write: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return conn, nil
}

// replay re-sends the retained handshake frames plus the undelivered
// frame (if any) to a freshly dialed endpoint, in original stream
// order, so the daemon hosts a complete MCP session.
func (s *shim) replay(conn net.Conn, pending []byte) error {
	buf := s.hs.replayBytes()
	buf = append(buf, pending...)
	if len(buf) == 0 {
		return nil
	}
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("daemon: shim replay write: %w", err)
	}
	return nil
}

// sessionOutcome reports why one piped session ended.
type sessionOutcome struct {
	// done means RunShim is finished and should return err.
	done bool
	err  error
	// pending is the complete, undelivered client frame whose write
	// failed when the daemon connection broke, when that frame is not
	// already retained in the handshake log. Re-sent after re-dial.
	pending []byte
}

// session pumps frames over conn until the client closes stdin, ctx is
// cancelled, or the daemon connection breaks. Both pump goroutines are
// joined before a re-dial outcome is returned so the next session is
// the only reader of stdin and the only writer of stdout.
func (s *shim) session(ctx context.Context, conn net.Conn, skipFirst bool) sessionOutcome {
	defer func() { _ = conn.Close() }()
	// Tie the connection to shutdown: closing it unblocks both pumps.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	connCh := make(chan connResult, 1)
	go func() { connCh <- s.pumpConn(conn, skipFirst) }()
	stdinCh := make(chan stdinResult, 1)
	go func() { stdinCh <- s.pumpStdin(conn) }()

	connDone := false
	for {
		select {
		case <-ctx.Done():
			// Operator shutdown (SIGINT/SIGTERM): a clean stop, like
			// the in-process server's ctx-cancel path.
			return sessionOutcome{done: true}

		case cres := <-connCh:
			connDone = true
			if cres.stdoutErr != nil {
				// The client side is gone; no endpoint can save this
				// session.
				return sessionOutcome{done: true, err: fmt.Errorf("daemon: shim write stdout: %w", cres.stdoutErr)}
			}
			// The daemon side ended. Close the conn so the stdin
			// pump's next write fails fast, then keep waiting: the
			// pump surfaces the broken conn (re-dial) or stdin EOF.
			s.log.Debug("guild daemon connection ended", "err", cres.connErr)
			_ = conn.Close()

		case sres := <-stdinCh:
			switch {
			case sres.err != nil:
				return sessionOutcome{done: true, err: fmt.Errorf("daemon: shim read stdin: %w", sres.err)}
			case sres.eof:
				// Client done: half-close toward the daemon, drain its
				// trailing responses, finish cleanly.
				s.drain(ctx, conn, connCh, connDone)
				return sessionOutcome{done: true}
			default: // conn broke under a client frame
				_ = conn.Close()
				if !connDone {
					select {
					case <-connCh:
					case <-ctx.Done():
						return sessionOutcome{done: true}
					}
				}
				return sessionOutcome{pending: sres.pending}
			}
		}
	}
}

// drain handles the clean-shutdown tail: the client closed stdin, so
// signal EOF to the daemon (half-close) and give its remaining
// responses a bounded window to flush to stdout.
func (s *shim) drain(ctx context.Context, conn net.Conn, connCh <-chan connResult, connDone bool) {
	if connDone {
		return
	}
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	} else {
		_ = conn.Close()
	}
	timer := time.NewTimer(shimDrainTimeout)
	defer timer.Stop()
	select {
	case <-connCh:
	case <-ctx.Done():
	case <-timer.C:
		s.log.Warn("guild daemon did not finish the session within the drain window")
	}
}

// stdinResult reports how one stdin->conn pump run ended. Exactly one
// of the conditions holds.
type stdinResult struct {
	// eof: the client closed stdin; the session is over.
	eof bool
	// err: reading stdin failed with a non-EOF error.
	err error
	// pending: set with neither eof nor err, meaning a conn write
	// failed; carries the undelivered frame unless it is already
	// retained for handshake replay.
	pending []byte
}

// pumpStdin forwards client frames to the daemon one complete line at
// a time. Per-line delivery is what makes recovery sound: a frame is
// either fully delivered to exactly one endpoint or carried over as
// pending, never split across endpoints. Bytes are forwarded verbatim;
// no JSON is parsed.
func (s *shim) pumpStdin(conn net.Conn) stdinResult {
	for {
		line, err := s.stdin.ReadBytes('\n')
		if err != nil {
			// A trailing fragment without '\n' is not a complete frame;
			// no endpoint could act on it, so it is dropped with the
			// stream.
			if errors.Is(err, io.EOF) {
				return stdinResult{eof: true}
			}
			return stdinResult{err: err}
		}
		retained := s.hs.retain(line)
		if _, werr := conn.Write(line); werr != nil {
			res := stdinResult{}
			if !retained {
				res.pending = line
			}
			return res
		}
	}
}

// connResult reports how one conn->stdout pump run ended. Exactly one
// field is set.
type connResult struct {
	// stdoutErr: writing a frame to the client failed.
	stdoutErr error
	// connErr: the daemon connection ended (EOF, reset, or local
	// close).
	connErr error
}

// pumpConn forwards daemon frames to the client one complete line at a
// time, so stdout always sits at a frame boundary: if the daemon dies
// mid-frame the partial tail is discarded, never emitted. skipFirst
// drops the first complete frame, the duplicate initialize response
// produced by a handshake replay.
func (s *shim) pumpConn(conn net.Conn, skipFirst bool) connResult {
	br := bufio.NewReader(conn)
	skip := skipFirst
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			return connResult{connErr: err}
		}
		if skip {
			skip = false
			continue
		}
		if _, werr := s.cfg.Stdout.Write(line); werr != nil {
			return connResult{stdoutErr: werr}
		}
		s.respSeen = true
	}
}

// fallback hands the session to the in-process server: the retained
// handshake (and any undelivered frame) is replayed ahead of the
// remaining stdin, and the duplicate initialize response is suppressed
// when the client already received the original.
func (s *shim) fallback(ctx context.Context, pending []byte) error {
	if s.cfg.Fallback == nil {
		return errors.New("daemon: shim lost the daemon and no in-process fallback is configured")
	}
	replay := s.hs.replayBytes()
	replay = append(replay, pending...)
	r := io.MultiReader(bytes.NewReader(replay), s.stdin)
	var w io.Writer = s.cfg.Stdout
	if s.respSeen && s.hs.count() > 0 {
		w = &dropFirstLineWriter{dst: s.cfg.Stdout}
	}
	s.log.Warn("guild daemon session continuing in-process")
	return s.cfg.Fallback(ctx, r, w)
}

// handshakeLog retains the first handshakeFrameCount client frames
// verbatim. These are, per the MCP lifecycle, the initialize request
// and the initialized notification: the only frames a fresh endpoint
// needs to host an already-initialized client session.
type handshakeLog struct {
	frames [][]byte
}

// retain records line if the log is not yet full, reporting whether
// the line is now retained (and therefore covered by every replay).
func (h *handshakeLog) retain(line []byte) bool {
	if len(h.frames) >= handshakeFrameCount {
		return false
	}
	h.frames = append(h.frames, slices.Clone(line))
	return true
}

// count returns how many frames are retained.
func (h *handshakeLog) count() int { return len(h.frames) }

// replayBytes returns the retained frames concatenated in stream
// order. The returned slice is freshly allocated; callers may append.
func (h *handshakeLog) replayBytes() []byte {
	var out []byte
	for _, f := range h.frames {
		out = append(out, f...)
	}
	return out
}

// dropFirstLineWriter suppresses everything up to and including the
// first '\n' written through it, then passes bytes through verbatim.
// It absorbs the duplicate initialize response the in-process fallback
// server produces when the handshake is replayed.
type dropFirstLineWriter struct {
	dst     io.Writer
	dropped bool
}

func (w *dropFirstLineWriter) Write(p []byte) (int, error) {
	if w.dropped {
		return w.dst.Write(p)
	}
	i := bytes.IndexByte(p, '\n')
	if i < 0 {
		// Still inside the suppressed line: swallow, report consumed.
		return len(p), nil
	}
	w.dropped = true
	rest := p[i+1:]
	if len(rest) > 0 {
		if n, err := w.dst.Write(rest); err != nil {
			return i + 1 + n, err
		}
	}
	return len(p), nil
}

// marshalShimPreamble renders the newline-terminated wire form of a
// shim preamble: {"guild_shim":{...}}. It marshals the same wire
// struct readPreamble parses, so the two sides cannot drift.
func marshalShimPreamble(p ShimPreamble) ([]byte, error) {
	data, err := json.Marshal(preamble{Shim: &p})
	if err != nil {
		return nil, fmt.Errorf("daemon: marshal shim preamble: %w", err)
	}
	return append(data, '\n'), nil
}
