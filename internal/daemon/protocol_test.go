package daemon

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestReadPreamble_Shim(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(
		`{"guild_shim":{"version":"v0.3.2","cwd":"/work/proj","pid":4242}}` + "\n"))
	p, err := readPreamble(br)
	if err != nil {
		t.Fatalf("readPreamble: %v", err)
	}
	if p.Shim == nil || p.StatusRequest != nil {
		t.Fatalf("want shim-only preamble; got shim=%v status=%v", p.Shim, p.StatusRequest)
	}
	if p.Shim.Version != "v0.3.2" || p.Shim.CWD != "/work/proj" || p.Shim.PID != 4242 {
		t.Fatalf("shim fields = %+v", *p.Shim)
	}
}

func TestReadPreamble_StatusRequest(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(`{"guild_status_request":{}}` + "\n"))
	p, err := readPreamble(br)
	if err != nil {
		t.Fatalf("readPreamble: %v", err)
	}
	if p.StatusRequest == nil || p.Shim != nil {
		t.Fatalf("want status-only preamble; got shim=%v status=%v", p.Shim, p.StatusRequest)
	}
}

func TestReadPreamble_Rejects(t *testing.T) {
	cases := map[string]string{
		"not json":        "this is not json\n",
		"neither field":   `{"hello":"world"}` + "\n",
		"both fields":     `{"guild_shim":{"version":"v","cwd":"/x","pid":1},"guild_status_request":{}}` + "\n",
		"zero pid":        `{"guild_shim":{"version":"v","cwd":"/x","pid":0}}` + "\n",
		"negative pid":    `{"guild_shim":{"version":"v","cwd":"/x","pid":-7}}` + "\n",
		"empty cwd":       `{"guild_shim":{"version":"v","cwd":"","pid":1}}` + "\n",
		"eof before line": "",
		"unterminated":    `{"guild_status_request":{}}`, // no trailing newline
		"oversized": `{"guild_shim":{"version":"` +
			strings.Repeat("x", preambleMaxBytes) + `","cwd":"/x","pid":1}}` + "\n",
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

// TestSessionConn_DrainsBufferedRemainder pins the splice contract: any
// bytes the preamble's bufio.Reader over-read (they arrived in the same
// packet as the preamble line) must surface to the session reader
// before fresh conn reads.
func TestSessionConn_DrainsBufferedRemainder(t *testing.T) {
	payload := `{"guild_status_request":{}}` + "\n" + `{"jsonrpc":"2.0","id":1}` + "\n"
	br := bufio.NewReader(strings.NewReader(payload))
	if _, err := readPreamble(br); err != nil {
		t.Fatalf("readPreamble: %v", err)
	}

	sc := &sessionConn{r: br, conn: nil} // Write/Close unused in this test
	rest, err := io.ReadAll(sc)
	if err != nil {
		t.Fatalf("read remainder: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":1}` + "\n"
	if !bytes.Equal(rest, []byte(want)) {
		t.Fatalf("remainder = %q; want %q", rest, want)
	}
}
