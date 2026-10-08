package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type jxIn struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

type jxOut struct {
	Greeting string `json:"greeting"`
	Count    int    `json:"count"`
	Hidden   string `json:"-"`
}

// RestoreInput re-attaches the wire-dropped Hidden field from the local
// input, mirroring quest.ListOutput's hook.
func (o *jxOut) RestoreInput(in jxIn) { o.Hidden = in.Name }

var errJXBoom = errors.New("boom: handler failed")

func newJXCommand(t *testing.T, calls *int) *Command[jxIn, jxOut] {
	t.Helper()
	return &Command[jxIn, jxOut]{
		Name:    "jx_test",
		CLIPath: []string{"jx", "test"},
		Short:   "test verb",
		Handler: func(_ context.Context, _ Deps, in jxIn) (jxOut, error) {
			if calls != nil {
				*calls++
			}
			if in.Name == "boom" {
				return jxOut{}, WithHint(errJXBoom, "try not exploding")
			}
			return jxOut{Greeting: "hello " + in.Name, Count: in.Count + 1, Hidden: in.Name}, nil
		},
		CLIFormat: func(s CLISink, o jxOut) string {
			return s.Line("✨", "[jx]", fmt.Sprintf("%s (%d) hidden=%s", o.Greeting, o.Count, o.Hidden))
		},
		MCPFormat: func(_ MCPSink, o jxOut) string { return o.Greeting },
		CLIErrorFormat: func(s CLISink, err error) (string, bool) {
			if errors.Is(err, errJXBoom) {
				return s.Line("❌", "[err]", "narrated: "+err.Error()), true
			}
			return "", false
		},
	}
}

func TestExecRegistry_SuccessRoundTrip(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	reg := NewExecRegistry()
	gotCWD := ""
	RegisterExec(reg, cmd, func(_ context.Context, cwd string) Deps {
		gotCWD = cwd
		return Deps{}
	})

	res, herr, err := reg.Exec(context.Background(), "jx_test", "/work/p", false,
		json.RawMessage(`{"name":"world","count":2}`))
	if err != nil || herr != nil {
		t.Fatalf("Exec: res=%s herr=%+v err=%v", res, herr, err)
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times; want 1", calls)
	}
	if gotCWD != "/work/p" {
		t.Fatalf("DepsBuilder cwd = %q; want /work/p", gotCWD)
	}
	var out jxOut
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if out.Greeting != "hello world" || out.Count != 3 {
		t.Fatalf("decoded out = %+v", out)
	}
	if out.Hidden != "" {
		t.Fatalf("json:\"-\" field crossed the wire: %q", out.Hidden)
	}
}

func TestExecRegistry_HandlerErrorCarriesHintAndNarration(t *testing.T) {
	cmd := newJXCommand(t, nil)
	reg := NewExecRegistry()
	RegisterExec(reg, cmd, func(context.Context, string) Deps { return Deps{} })

	res, herr, err := reg.Exec(context.Background(), "jx_test", "/w", true,
		json.RawMessage(`{"name":"boom"}`))
	if err != nil {
		t.Fatalf("dispatch error: %v", err)
	}
	if res != nil {
		t.Fatalf("result on handler error: %s", res)
	}
	if herr == nil {
		t.Fatal("want handler error")
	}
	if herr.Message != errJXBoom.Error() {
		t.Errorf("Message = %q; want %q", herr.Message, errJXBoom.Error())
	}
	if herr.Hint != "try not exploding" {
		t.Errorf("Hint = %q", herr.Hint)
	}
	if !herr.NarrationOK || !strings.Contains(herr.Narration, "[err] narrated: boom") {
		t.Errorf("Narration = %q ok=%v; want ASCII narration (NoEmoji=true was sent)", herr.Narration, herr.NarrationOK)
	}
}

func TestExecRegistry_DispatchFailuresDoNotRunHandler(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	reg := NewExecRegistry()
	RegisterExec(reg, cmd, func(context.Context, string) Deps { return Deps{} })

	if _, _, err := reg.Exec(context.Background(), "jx_missing", "/w", false, nil); err == nil {
		t.Fatal("unknown verb accepted")
	}
	if _, _, err := reg.Exec(context.Background(), "jx_test", "/w", false,
		json.RawMessage(`{"name":`)); err == nil {
		t.Fatal("undecodable args accepted")
	}
	if calls != 0 {
		t.Fatalf("handler ran %d times on dispatch failures; want 0", calls)
	}
}

func TestExecRegistry_NullAndEmptyArgsMeanZeroInput(t *testing.T) {
	cmd := newJXCommand(t, nil)
	reg := NewExecRegistry()
	RegisterExec(reg, cmd, func(context.Context, string) Deps { return Deps{} })

	for _, args := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("{}")} {
		res, herr, err := reg.Exec(context.Background(), "jx_test", "/w", false, args)
		if err != nil || herr != nil {
			t.Fatalf("args=%q: herr=%+v err=%v", args, herr, err)
		}
		var out jxOut
		if err := json.Unmarshal(res, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Greeting != "hello " || out.Count != 1 {
			t.Fatalf("args=%q: out=%+v; want zero-input handling", args, out)
		}
	}
}

func TestRegisterExec_SkipsExemptVerbs(t *testing.T) {
	reason, exempt := ExecExemptionReason("quest_orders")
	if !exempt || reason == "" {
		t.Fatal("quest_orders must be exec-exempt with a documented reason")
	}

	exemptCmd := newJXCommand(t, nil)
	exemptCmd.Name = "quest_orders"
	reg := NewExecRegistry()
	RegisterExec(reg, exemptCmd, func(context.Context, string) Deps { return Deps{} })
	if _, _, err := reg.Exec(context.Background(), "quest_orders", "/w", false, nil); err == nil {
		t.Fatal("exempt verb was registered and executed daemon-side")
	}
	if names := reg.Names(); len(names) != 0 {
		t.Fatalf("registry names = %v; want empty", names)
	}
}

func TestDispatchHandler_RemoteSuccessSkipsLocalAndRestoresInput(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	remoteCalls := 0
	d := Deps{
		ExecRemote: func(_ context.Context, req RemoteExecRequest) (json.RawMessage, error) {
			remoteCalls++
			if req.Tool != "jx_test" {
				t.Fatalf("req.Tool = %q", req.Tool)
			}
			if !req.NoEmoji {
				t.Fatal("NoEmoji not threaded through")
			}
			var in jxIn
			if err := json.Unmarshal(req.Args, &in); err != nil {
				t.Fatalf("remote decode args: %v", err)
			}
			return json.Marshal(jxOut{Greeting: "hello " + in.Name, Count: in.Count + 1, Hidden: in.Name})
		},
	}

	out, err := cmd.dispatchHandler(context.Background(), d, jxIn{Name: "remote", Count: 5}, true)
	if err != nil {
		t.Fatalf("dispatchHandler: %v", err)
	}
	if calls != 0 {
		t.Fatalf("local handler ran %d times on remote success; want 0", calls)
	}
	if remoteCalls != 1 {
		t.Fatalf("remote ran %d times; want 1", remoteCalls)
	}
	if out.Greeting != "hello remote" || out.Count != 6 {
		t.Fatalf("out = %+v", out)
	}
	// Wire drops Hidden (json:"-"); RestoreInput must re-attach it from
	// the LOCAL input so rendering stays byte-identical.
	if out.Hidden != "remote" {
		t.Fatalf("RestoreInput not applied: Hidden = %q", out.Hidden)
	}
}

func TestDispatchHandler_TransportErrorFallsBackLocal(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	d := Deps{
		ExecRemote: func(context.Context, RemoteExecRequest) (json.RawMessage, error) {
			return nil, errors.New("conn dropped")
		},
	}
	out, err := cmd.dispatchHandler(context.Background(), d, jxIn{Name: "x"}, false)
	if err != nil {
		t.Fatalf("dispatchHandler: %v", err)
	}
	if calls != 1 {
		t.Fatalf("local handler ran %d times; want 1 (fallback)", calls)
	}
	if out.Greeting != "hello x" {
		t.Fatalf("out = %+v", out)
	}
}

func TestDispatchHandler_RemoteHandlerErrorIsFinal(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	want := &RemoteHandlerError{Message: "boom: handler failed", Hint: "h", Narration: "n", NarrationOK: true}
	d := Deps{
		ExecRemote: func(context.Context, RemoteExecRequest) (json.RawMessage, error) {
			return nil, want
		},
	}
	_, err := cmd.dispatchHandler(context.Background(), d, jxIn{Name: "x"}, false)
	if calls != 0 {
		t.Fatalf("local handler re-ran after remote handler error: %d", calls)
	}
	var got *RemoteHandlerError
	if !errors.As(err, &got) || got != want {
		t.Fatalf("err = %v; want the RemoteHandlerError back", err)
	}
	if got.Error() != "boom: handler failed" || got.AgentHint() != "h" {
		t.Fatalf("remote error surface = %q / %q", got.Error(), got.AgentHint())
	}
	if AgentHintFromError(err) != "h" {
		t.Fatalf("AgentHintFromError = %q; want h", AgentHintFromError(err))
	}
}

func TestDispatchHandler_NilExecRemoteAndExemptRunLocal(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	if _, err := cmd.dispatchHandler(context.Background(), Deps{}, jxIn{Name: "a"}, false); err != nil {
		t.Fatalf("nil ExecRemote: %v", err)
	}

	exemptCmd := newJXCommand(t, &calls)
	exemptCmd.Name = "quest_orders"
	d := Deps{
		ExecRemote: func(context.Context, RemoteExecRequest) (json.RawMessage, error) {
			t.Fatal("exempt verb reached the remote transport")
			return nil, nil
		},
	}
	if _, err := exemptCmd.dispatchHandler(context.Background(), d, jxIn{Name: "b"}, false); err != nil {
		t.Fatalf("exempt dispatch: %v", err)
	}
	if calls != 2 {
		t.Fatalf("local handler ran %d times; want 2", calls)
	}
}

func TestDispatchHandler_UndecodableRemoteResultDoesNotReRun(t *testing.T) {
	calls := 0
	cmd := newJXCommand(t, &calls)
	d := Deps{
		ExecRemote: func(context.Context, RemoteExecRequest) (json.RawMessage, error) {
			return json.RawMessage(`{"greeting":`), nil
		},
	}
	if _, err := cmd.dispatchHandler(context.Background(), d, jxIn{Name: "x"}, false); err == nil {
		t.Fatal("undecodable remote result accepted")
	}
	// The verb already executed in the daemon: a local re-run could
	// double-apply writes, so the local handler must NOT run.
	if calls != 0 {
		t.Fatalf("local handler ran %d times after remote success; want 0", calls)
	}
}
