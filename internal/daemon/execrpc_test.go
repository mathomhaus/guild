//go:build unix

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// startExecDaemon spins up a daemon whose Exec handler is fn, returning
// the socket path and a buffer capturing the daemon's structured log.
func startExecDaemon(t *testing.T, fn ExecHandler) (string, *syncBuffer) {
	t.Helper()
	setHome(t)
	sock := shortSocketPath(t)

	logBuf := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv, err := NewServer(Config{
		Version:    "v-exec",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Exec:       fn,
		Logger:     slog.New(slog.NewTextHandler(logBuf, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)
	return sock, logBuf
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing daemon logs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestExecCall_SuccessRoundTrip(t *testing.T) {
	var gotReq ExecRequest
	sock, logBuf := startExecDaemon(t, func(_ context.Context, req ExecRequest) (json.RawMessage, *ExecHandlerError, error) {
		gotReq = req
		return json.RawMessage(`{"quest":{"id":"QUEST-1"}}`), nil, nil
	})

	pre := ExecPreamble{Tool: "quest_post", Version: "v-exec", CWD: "/work/p", PID: 4242, NoEmoji: true}
	res, herr, err := ExecCall(context.Background(), sock, pre, json.RawMessage(`{"subject":"hi"}`))
	if err != nil || herr != nil {
		t.Fatalf("ExecCall: res=%s herr=%+v err=%v", res, herr, err)
	}
	if string(res) != `{"quest":{"id":"QUEST-1"}}` {
		t.Fatalf("result = %s", res)
	}
	if gotReq.Tool != "quest_post" || gotReq.CWD != "/work/p" || !gotReq.NoEmoji {
		t.Fatalf("daemon saw req %+v", gotReq)
	}
	if !bytes.Equal(bytes.TrimSpace(gotReq.Args), []byte(`{"subject":"hi"}`)) {
		t.Fatalf("daemon saw args %q", gotReq.Args)
	}

	// The daemon-side execution record: tool, cwd, pid, outcome.
	log := logBuf.String()
	for _, want := range []string{"daemon: exec", "tool=quest_post", "cwd=/work/p", "client_pid=4242", "outcome=ok"} {
		if !strings.Contains(log, want) {
			t.Errorf("daemon log missing %q:\n%s", want, log)
		}
	}
}

func TestExecCall_HandlerErrorIsFinalShape(t *testing.T) {
	sock, _ := startExecDaemon(t, func(context.Context, ExecRequest) (json.RawMessage, *ExecHandlerError, error) {
		return nil, &ExecHandlerError{
			Message: "already accepted", Hint: "pick another", Narration: "❌ already accepted", NarrationOK: true,
		}, nil
	})

	pre := ExecPreamble{Tool: "quest_accept", Version: "v-exec", CWD: "/w", PID: 1}
	res, herr, err := ExecCall(context.Background(), sock, pre, nil)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res != nil {
		t.Fatalf("result on handler error: %s", res)
	}
	if herr == nil || herr.Message != "already accepted" || herr.Hint != "pick another" ||
		!herr.NarrationOK || herr.Narration != "❌ already accepted" {
		t.Fatalf("handler error = %+v", herr)
	}
}

func TestExecCall_DispatchErrorIsPlainError(t *testing.T) {
	sock, _ := startExecDaemon(t, func(_ context.Context, req ExecRequest) (json.RawMessage, *ExecHandlerError, error) {
		return nil, nil, fmt.Errorf("guild_exec: unknown verb %q", req.Tool)
	})

	pre := ExecPreamble{Tool: "quest_nonsense", Version: "v-exec", CWD: "/w", PID: 1}
	_, herr, err := ExecCall(context.Background(), sock, pre, nil)
	if herr != nil {
		t.Fatalf("handler error on dispatch failure: %+v", herr)
	}
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("err = %v; want unknown-verb dispatch error", err)
	}
}

func TestExecCall_VersionSkewRefusedBeforeHandler(t *testing.T) {
	handlerRan := false
	sock, logBuf := startExecDaemon(t, func(context.Context, ExecRequest) (json.RawMessage, *ExecHandlerError, error) {
		handlerRan = true
		return json.RawMessage(`{}`), nil, nil
	})

	pre := ExecPreamble{Tool: "quest_post", Version: "v-other", CWD: "/w", PID: 1}
	_, herr, err := ExecCall(context.Background(), sock, pre, nil)
	if herr != nil {
		t.Fatalf("handler error on skew: %+v", herr)
	}
	if err == nil || !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("err = %v; want version-mismatch refusal", err)
	}
	if handlerRan {
		t.Fatal("handler ran despite version skew")
	}
	if !strings.Contains(logBuf.String(), "exec refused on version skew") {
		t.Errorf("daemon log missing skew refusal:\n%s", logBuf.String())
	}
}

func TestExecCall_NilExecHandlerRefused(t *testing.T) {
	setHome(t)
	sock := shortSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := NewServer(Config{
		Version:    "v-noexec",
		SocketPath: sock,
		Sessions:   blockingHandler,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	startDaemon(t, ctx, srv, sock)

	pre := ExecPreamble{Tool: "quest_post", Version: "v-noexec", CWD: "/w", PID: 1}
	_, herr, callErr := ExecCall(context.Background(), sock, pre, nil)
	if herr != nil {
		t.Fatalf("handler error: %+v", herr)
	}
	if callErr == nil || !strings.Contains(callErr.Error(), "not supported") {
		t.Fatalf("err = %v; want exec-not-supported refusal", callErr)
	}
}

func TestExecCall_ConnClosesAfterOneExchange(t *testing.T) {
	sock, _ := startExecDaemon(t, func(context.Context, ExecRequest) (json.RawMessage, *ExecHandlerError, error) {
		return json.RawMessage(`{}`), nil, nil
	})

	// Drive the wire by hand to observe the close after one response.
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	payload := `{"guild_exec":{"tool":"x","version":"v-exec","cwd":"/w","pid":1}}` + "\n{}\n"
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(conn)
	if _, err := br.ReadBytes('\n'); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("conn still open after one exec exchange")
	}
}

func TestReadPreamble_ExecVariant(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(
		`{"guild_exec":{"tool":"quest_post","version":"v1","cwd":"/p","pid":7,"no_emoji":true}}` + "\n"))
	p, err := readPreamble(br)
	if err != nil {
		t.Fatalf("readPreamble: %v", err)
	}
	if p.Exec == nil || p.Shim != nil || p.StatusRequest != nil {
		t.Fatalf("want exec-only preamble; got %+v", p)
	}
	if p.Exec.Tool != "quest_post" || p.Exec.CWD != "/p" || p.Exec.PID != 7 || !p.Exec.NoEmoji {
		t.Fatalf("exec fields = %+v", *p.Exec)
	}
}

func TestReadPreamble_ExecRejects(t *testing.T) {
	cases := map[string]string{
		"empty tool":       `{"guild_exec":{"tool":"","version":"v","cwd":"/p","pid":1}}` + "\n",
		"empty cwd":        `{"guild_exec":{"tool":"x","version":"v","cwd":"","pid":1}}` + "\n",
		"zero pid":         `{"guild_exec":{"tool":"x","version":"v","cwd":"/p","pid":0}}` + "\n",
		"exec plus shim":   `{"guild_exec":{"tool":"x","version":"v","cwd":"/p","pid":1},"guild_shim":{"version":"v","cwd":"/p","pid":1}}` + "\n",
		"exec plus status": `{"guild_exec":{"tool":"x","version":"v","cwd":"/p","pid":1},"guild_status_request":{}}` + "\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			br := bufio.NewReader(strings.NewReader(input))
			if _, err := readPreamble(br); err == nil {
				t.Fatalf("readPreamble accepted %q", name)
			}
		})
	}
}

func TestExecCall_DialFailureIsTransportError(t *testing.T) {
	pre := ExecPreamble{Tool: "x", Version: "v", CWD: "/w", PID: 1}
	_, herr, err := ExecCall(context.Background(), "/nonexistent/g.sock", pre, nil)
	if herr != nil {
		t.Fatalf("handler error: %+v", herr)
	}
	if err == nil {
		t.Fatal("dial to missing socket succeeded")
	}
}
