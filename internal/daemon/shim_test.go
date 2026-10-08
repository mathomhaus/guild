//go:build unix

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Frames with deliberately irregular spacing: the raw-frame ruling says
// the shim forwards bytes verbatim, so any normalization (re-marshal,
// trim, re-indent) fails these tests.
const (
	frameInit   = `{ "jsonrpc" :"2.0","id":0,  "method":"initialize","params":{}}` + "\n"
	frameInited = `{"jsonrpc":"2.0",  "method" : "notifications/initialized"}` + "\n"
	frameCall   = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{ "name":"x" }}` + "\n"
	respInit    = `{"jsonrpc":"2.0" ,"id":0,"result":{"weird":   "spacing"}}` + "\n"
	respCall    = `{"jsonrpc":"2.0","id":1,"result":{   }}` + "\n"
	respInitDup = `{"jsonrpc":"2.0","id":0,"result":{"from":"the second endpoint"}}` + "\n"
)

// shimPipes wires RunShim to in-test stdio: the test writes client
// frames into stdin and reads piped frames from out. Returned shimErr
// receives RunShim's result.
func shimPipes(t *testing.T, ctx context.Context, cfg ShimConfig) (stdin *io.PipeWriter, out *bufio.Reader, shimErr chan error) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	cfg.Stdin = stdinR
	cfg.Stdout = stdoutW
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	shimErr = make(chan error, 1)
	go func() { shimErr <- RunShim(ctx, cfg) }()
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = stdoutR.Close()
	})
	return stdinW, bufio.NewReader(stdoutR), shimErr
}

// mustWrite writes a full frame into the shim's stdin.
func mustWrite(t *testing.T, w io.Writer, frame string) {
	t.Helper()
	if _, err := io.WriteString(w, frame); err != nil {
		t.Fatalf("write %q to shim stdin: %v", frame, err)
	}
}

// readLineTimeout reads one '\n'-terminated line from the shim's
// stdout, failing the test if none arrives within 5s (a hang here is
// the shim losing or withholding a frame).
func readLineTimeout(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := br.ReadString('\n')
		ch <- res{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read shim stdout: %v", r.err)
		}
		return r.line
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a frame on shim stdout")
		return ""
	}
}

// waitShim asserts RunShim returned and its error matched want (nil
// means "no error").
func waitShim(t *testing.T, shimErr chan error, want error) {
	t.Helper()
	select {
	case err := <-shimErr:
		if want == nil && err != nil {
			t.Fatalf("RunShim returned %v; want nil", err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Fatalf("RunShim returned %v; want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunShim did not return within 5s")
	}
}

func TestRunShim_RawPipeFullSession(t *testing.T) {
	sock := shortSocketPath(t)
	ln := listenUnix(t, sock)

	type recv struct {
		pre    ShimPreamble
		frames []string
	}
	srvGot := make(chan recv, 1)
	srvErr := make(chan error, 1)
	go func() {
		srvErr <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			pre, err := readPreamble(br)
			if err != nil {
				return err
			}
			f1, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			if _, err := conn.Write([]byte(respInit)); err != nil {
				return err
			}
			f2, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			f3, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			if _, err := conn.Write([]byte(respCall)); err != nil {
				return err
			}
			srvGot <- recv{pre: *pre.Shim, frames: []string{f1, f2, f3}}
			// The client closing stdin must reach us as EOF (the shim
			// half-closes), ending the session from this side too.
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				return fmt.Errorf("expected EOF after client close; got %v", err)
			}
			return nil
		}()
	}()

	stdin, out, shimErr := shimPipes(t, context.Background(), ShimConfig{
		SocketPath: sock,
		Preamble:   ShimPreamble{Version: "v-shim", CWD: "/work/proj", PID: 4242},
	})

	mustWrite(t, stdin, frameInit)
	if got := readLineTimeout(t, out); got != respInit {
		t.Fatalf("initialize response rewritten:\n got %q\nwant %q", got, respInit)
	}
	mustWrite(t, stdin, frameInited)
	mustWrite(t, stdin, frameCall)
	if got := readLineTimeout(t, out); got != respCall {
		t.Fatalf("tool response rewritten:\n got %q\nwant %q", got, respCall)
	}

	_ = stdin.Close()
	waitShim(t, shimErr, nil)
	if err := <-srvErr; err != nil {
		t.Fatalf("scripted daemon: %v", err)
	}

	rec := <-srvGot
	if rec.pre.Version != "v-shim" || rec.pre.CWD != "/work/proj" || rec.pre.PID != 4242 {
		t.Fatalf("daemon saw preamble %+v", rec.pre)
	}
	for i, want := range []string{frameInit, frameInited, frameCall} {
		if rec.frames[i] != want {
			t.Fatalf("client frame %d rewritten:\n got %q\nwant %q", i+1, rec.frames[i], want)
		}
	}
}

// TestRunShim_NoListener_StdioUntouched: a dial failure before any
// stdio traffic must surface as ErrShimUnavailable with stdin never
// read, so the caller can run the in-process server byte-identically.
func TestRunShim_NoListener_StdioUntouched(t *testing.T) {
	sock := shortSocketPath(t) // nothing listening here

	err := RunShim(context.Background(), ShimConfig{
		SocketPath: sock,
		Preamble:   ShimPreamble{Version: "v", CWD: "/x", PID: 1},
		Stdin: readerFunc(func([]byte) (int, error) {
			t.Error("shim read stdin despite never establishing a daemon session")
			return 0, io.EOF
		}),
		Stdout: io.Discard,
		Logger: quietLogger(),
	})
	if !errors.Is(err, ErrShimUnavailable) {
		t.Fatalf("RunShim = %v; want ErrShimUnavailable", err)
	}
}

type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestRunShim_DaemonLostMidSession_RedialReplaysHandshake(t *testing.T) {
	sock := shortSocketPath(t)
	ln := listenUnix(t, sock)

	conn1Gone := make(chan struct{})
	srv1Err := make(chan error, 1)
	go func() { // first daemon endpoint: handshake, then dies (SIGKILL analog)
		srv1Err <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			br := bufio.NewReader(conn)
			if _, err := readPreamble(br); err != nil {
				return err
			}
			if _, err := br.ReadString('\n'); err != nil { // initialize
				return err
			}
			if _, err := conn.Write([]byte(respInit)); err != nil {
				return err
			}
			if _, err := br.ReadString('\n'); err != nil { // initialized
				return err
			}
			if err := conn.Close(); err != nil {
				return err
			}
			close(conn1Gone)
			return nil
		}()
	}()

	stdin, out, shimErr := shimPipes(t, context.Background(), ShimConfig{
		SocketPath: sock,
		Preamble:   ShimPreamble{Version: "v-shim", CWD: "/work/proj", PID: 4243},
	})

	mustWrite(t, stdin, frameInit)
	if got := readLineTimeout(t, out); got != respInit {
		t.Fatalf("initialize response = %q; want %q", got, respInit)
	}
	mustWrite(t, stdin, frameInited)
	<-conn1Gone
	if err := <-srv1Err; err != nil {
		t.Fatalf("first endpoint: %v", err)
	}

	srv2Got := make(chan []string, 1)
	srv2Err := make(chan error, 1)
	go func() { // second endpoint: must see preamble + replayed handshake
		srv2Err <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			br := bufio.NewReader(conn)
			if _, err := readPreamble(br); err != nil {
				return err
			}
			frames := make([]string, 3)
			for i := range frames {
				if i == 2 {
					// Duplicate response to the replayed initialize,
					// sent before the tool call arrives: the shim must
					// suppress it.
					if _, err := conn.Write([]byte(respInitDup)); err != nil {
						return err
					}
				}
				if frames[i], err = br.ReadString('\n'); err != nil {
					return err
				}
			}
			if _, err := conn.Write([]byte(respCall)); err != nil {
				return err
			}
			srv2Got <- frames
			if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
				return fmt.Errorf("expected EOF after client close; got %v", err)
			}
			return nil
		}()
	}()

	// The next client frame hits the dead connection, which triggers
	// the single re-dial with handshake replay.
	mustWrite(t, stdin, frameCall)
	if got := readLineTimeout(t, out); got != respCall {
		t.Fatalf("frame after re-dial = %q; want %q (duplicate initialize response must be suppressed)", got, respCall)
	}

	got2 := <-srv2Got
	for i, want := range []string{frameInit, frameInited, frameCall} {
		if got2[i] != want {
			t.Fatalf("replayed frame %d:\n got %q\nwant %q", i+1, got2[i], want)
		}
	}

	_ = stdin.Close()
	waitShim(t, shimErr, nil)
	if err := <-srv2Err; err != nil {
		t.Fatalf("second endpoint: %v", err)
	}
}

func TestRunShim_FallbackAfterFailedRedial(t *testing.T) {
	sock := shortSocketPath(t)
	ln := listenUnix(t, sock)

	const (
		dupResp  = `{"jsonrpc":"2.0","id":0,"result":{"dup":true}}` + "\n"
		realResp = `{"jsonrpc":"2.0","id":1,"result":{"from":"fallback"}}` + "\n"
	)

	daemonGone := make(chan struct{})
	srvErr := make(chan error, 1)
	go func() { // one endpoint, then daemon and socket disappear entirely
		srvErr <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			br := bufio.NewReader(conn)
			if _, err := readPreamble(br); err != nil {
				return err
			}
			if _, err := br.ReadString('\n'); err != nil { // initialize
				return err
			}
			if _, err := conn.Write([]byte(respInit)); err != nil {
				return err
			}
			if err := conn.Close(); err != nil {
				return err
			}
			if err := ln.Close(); err != nil { // unlinks the socket: re-dial must fail
				return err
			}
			close(daemonGone)
			return nil
		}()
	}()

	fallbackSaw := make(chan []string, 1)
	fb := func(_ context.Context, r io.Reader, w io.Writer) error {
		br := bufio.NewReader(r)
		l1, err := br.ReadString('\n') // replayed initialize
		if err != nil {
			return err
		}
		l2, err := br.ReadString('\n') // the undelivered tool call
		if err != nil {
			return err
		}
		// First line out is the duplicate initialize response; the
		// client already has the original, so it must be suppressed.
		if _, err := io.WriteString(w, dupResp); err != nil {
			return err
		}
		if _, err := io.WriteString(w, realResp); err != nil {
			return err
		}
		fallbackSaw <- []string{l1, l2}
		// Serve until the client closes stdin.
		if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
			return fmt.Errorf("expected EOF; got %v", err)
		}
		return nil
	}

	stdin, out, shimErr := shimPipes(t, context.Background(), ShimConfig{
		SocketPath: sock,
		Preamble:   ShimPreamble{Version: "v-shim", CWD: "/work/proj", PID: 4244},
		Fallback:   fb,
	})

	mustWrite(t, stdin, frameInit)
	if got := readLineTimeout(t, out); got != respInit {
		t.Fatalf("initialize response = %q; want %q", got, respInit)
	}
	<-daemonGone
	if err := <-srvErr; err != nil {
		t.Fatalf("scripted daemon: %v", err)
	}

	mustWrite(t, stdin, frameCall)
	if got := readLineTimeout(t, out); got != realResp {
		t.Fatalf("first fallback frame = %q; want %q (duplicate initialize response must be suppressed)", got, realResp)
	}
	saw := <-fallbackSaw
	if saw[0] != frameInit || saw[1] != frameCall {
		t.Fatalf("fallback stream = %q; want replayed initialize then the undelivered call", saw)
	}

	_ = stdin.Close()
	waitShim(t, shimErr, nil)
}

// TestRunShim_LostDaemonWithoutFallback: with no Fallback configured a
// lost daemon plus failed re-dial must end the session with an error,
// never hang.
func TestRunShim_LostDaemonWithoutFallback(t *testing.T) {
	sock := shortSocketPath(t)
	ln := listenUnix(t, sock)

	srvErr := make(chan error, 1)
	daemonGone := make(chan struct{})
	go func() {
		srvErr <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			br := bufio.NewReader(conn)
			if _, err := readPreamble(br); err != nil {
				return err
			}
			if _, err := br.ReadString('\n'); err != nil {
				return err
			}
			_ = conn.Close()
			_ = ln.Close()
			close(daemonGone)
			return nil
		}()
	}()

	stdin, _, shimErr := shimPipes(t, context.Background(), ShimConfig{
		SocketPath: sock,
		Preamble:   ShimPreamble{Version: "v", CWD: "/x", PID: 7},
	})
	mustWrite(t, stdin, frameInit)
	<-daemonGone
	if err := <-srvErr; err != nil {
		t.Fatalf("scripted daemon: %v", err)
	}
	mustWrite(t, stdin, frameCall)

	select {
	case err := <-shimErr:
		if err == nil {
			t.Fatal("RunShim returned nil after losing the daemon with no fallback")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunShim hung after losing the daemon with no fallback")
	}
}

func TestRunShim_CtxCancelStopsCleanly(t *testing.T) {
	sock := shortSocketPath(t)
	ln := listenUnix(t, sock)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold the session open; never respond.
		_, _ = io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	_, _, shimErr := shimPipes(t, ctx, ShimConfig{
		SocketPath: sock,
		Preamble:   ShimPreamble{Version: "v", CWD: "/x", PID: 9},
	})

	// Idle session (no frames at all): cancellation alone must end it.
	cancel()
	waitShim(t, shimErr, nil)
}

func TestHandshakeLog_RetainsExactlyTwoFrames(t *testing.T) {
	var hs handshakeLog
	lines := []string{frameInit, frameInited, frameCall}
	wantRetained := []bool{true, true, false}
	for i, l := range lines {
		if got := hs.retain([]byte(l)); got != wantRetained[i] {
			t.Fatalf("retain(frame %d) = %v; want %v", i+1, got, wantRetained[i])
		}
	}
	if hs.count() != 2 {
		t.Fatalf("count = %d; want 2", hs.count())
	}
	if got := string(hs.replayBytes()); got != frameInit+frameInited {
		t.Fatalf("replayBytes = %q; want the first two frames verbatim", got)
	}
}

func TestDropFirstLineWriter_SplitWrites(t *testing.T) {
	var dst bytes.Buffer
	w := &dropFirstLineWriter{dst: &dst}

	// The suppressed line arrives in three fragments; the keeper frame
	// is split across the same writes.
	for _, chunk := range []string{`{"id":0,"resu`, `lt":{}}`, "\n" + `{"id":1,`, `"result":{}}` + "\n"} {
		n, err := w.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("Write(%q): %v", chunk, err)
		}
		if n != len(chunk) {
			t.Fatalf("Write(%q) consumed %d of %d bytes", chunk, n, len(chunk))
		}
	}
	want := `{"id":1,"result":{}}` + "\n"
	if dst.String() != want {
		t.Fatalf("passed-through bytes = %q; want %q", dst.String(), want)
	}
}

func TestMarshalShimPreamble_RoundTripsThroughReadPreamble(t *testing.T) {
	in := ShimPreamble{Version: "v0.9.9", CWD: "/some/dir", PID: 31337}
	line, err := marshalShimPreamble(in)
	if err != nil {
		t.Fatalf("marshalShimPreamble: %v", err)
	}
	if !bytes.HasSuffix(line, []byte("\n")) {
		t.Fatalf("preamble line not newline-terminated: %q", line)
	}
	if strings.Contains(string(line), "guild_status_request") {
		t.Fatalf("shim preamble leaks the status key: %q", line)
	}
	got, err := readPreamble(bufio.NewReader(bytes.NewReader(line)))
	if err != nil {
		t.Fatalf("readPreamble rejected the shim's own preamble: %v", err)
	}
	if got.Shim == nil || *got.Shim != in {
		t.Fatalf("round-trip = %+v; want %+v", got.Shim, in)
	}
}
