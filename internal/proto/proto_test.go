package proto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// parseResult decodes the single tool_result frame written to the buffer.
func parseResult(t *testing.T, buf *bytes.Buffer) struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	IsError bool   `json:"is_error"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		MimeType string `json:"mime_type"`
		Data     string `json:"data"`
	} `json:"content"`
} {
	t.Helper()
	var f struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		IsError bool   `json:"is_error"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			MimeType string `json:"mime_type"`
			Data     string `json:"data"`
		} `json:"content"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &f); err != nil {
		t.Fatalf("unmarshal tool_result: %v (raw: %s)", err, buf.String())
	}
	if f.Type != "tool_result" {
		t.Errorf("type = %q, want tool_result", f.Type)
	}
	return f
}

func TestSendTextResult(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	e.sendToolResult("call-1", Text("hello"))

	f := parseResult(t, &buf)
	if f.ID != "call-1" || f.IsError {
		t.Errorf("id=%q is_error=%v", f.ID, f.IsError)
	}
	if len(f.Content) != 1 || f.Content[0].Type != "text" || f.Content[0].Text != "hello" {
		t.Errorf("content = %+v, want one text block 'hello'", f.Content)
	}
}

func TestSendErrorResult(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	e.sendToolResult("call-2", Errorf("boom %d", 7))

	f := parseResult(t, &buf)
	if !f.IsError {
		t.Error("is_error = false, want true")
	}
	if len(f.Content) != 1 || f.Content[0].Text != "boom 7" {
		t.Errorf("content = %+v, want one text block 'boom 7'", f.Content)
	}
}

func TestSendImageResultWithCaption(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	raw := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	e.sendToolResult("call-3", Image("image/png", raw, "a logo"))

	f := parseResult(t, &buf)
	if f.IsError {
		t.Error("is_error = true, want false")
	}
	if len(f.Content) != 2 {
		t.Fatalf("content = %+v, want image + text blocks", f.Content)
	}
	img := f.Content[0]
	if img.Type != "image" || img.MimeType != "image/png" {
		t.Errorf("first block = %+v, want image/png image block", img)
	}
	got, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("data is not std-base64: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("decoded data = %v, want %v", got, raw)
	}
	if f.Content[1].Type != "text" || f.Content[1].Text != "a logo" {
		t.Errorf("second block = %+v, want text caption", f.Content[1])
	}
}

// runHandshake feeds the given host frames (a hello_ack and any follow-on
// frames, each a JSON line), then a shutdown, through a fresh Extension that has
// one network-read tool registered. It returns the Extension (for Host/Session/
// CWD assertions) and the frames the extension emitted on the wire.
func runHandshake(t *testing.T, hostFrames ...string) (*Extension, []map[string]json.RawMessage) {
	t.Helper()
	var out bytes.Buffer
	in := strings.Join(append(hostFrames, `{"type":"shutdown"}`), "\n") + "\n"
	e := &Extension{
		name: "web",
		in:   strings.NewReader(in),
		out:  &out,
	}
	e.Tool("web_fetch", "fetch a page", json.RawMessage(`{"type":"object"}`),
		func(json.RawMessage) Result { return Text("") }, NetworkRead())
	if err := e.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var frames []map[string]json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("unmarshal emitted frame %q: %v", line, err)
		}
		frames = append(frames, f)
	}
	return e, frames
}

func frameType(f map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(f["type"], &s)
	return s
}

func TestHelloAckZotHost(t *testing.T) {
	// An old zot host sends zot_version but no terva_version.
	e, _ := runHandshake(t, `{"type":"hello_ack","protocol_version":1,"zot_version":"0.103.2","data_dir":"/tmp/x"}`)
	host := e.Host()
	if host.IsTerva() {
		t.Errorf("IsTerva() = true for a zot-only ack; want false (terva_version=%q)", host.TervaVersion)
	}
	if host.ZotVersion != "0.103.2" {
		t.Errorf("ZotVersion = %q, want 0.103.2", host.ZotVersion)
	}
}

func TestHelloAckTervaHost(t *testing.T) {
	// A terva host adds terva_version while keeping zot_version for compat.
	e, _ := runHandshake(t, `{"type":"hello_ack","protocol_version":2,"zot_version":"0.103.x","terva_version":"0.104.0","data_dir":"/tmp/x"}`)
	host := e.Host()
	if !host.IsTerva() {
		t.Error("IsTerva() = false for a terva ack; want true")
	}
	if host.TervaVersion != "0.104.0" {
		t.Errorf("TervaVersion = %q, want 0.104.0", host.TervaVersion)
	}
}

func TestRegisterToolCarriesAuthority(t *testing.T) {
	_, frames := runHandshake(t, `{"type":"hello_ack","zot_version":"0.103.2"}`)
	var found bool
	for _, f := range frames {
		if frameType(f) != "register_tool" {
			continue
		}
		found = true
		var auth string
		_ = json.Unmarshal(f["authority"], &auth)
		if auth != "network-read" {
			t.Errorf("register_tool authority = %q, want network-read", auth)
		}
	}
	if !found {
		t.Fatal("no register_tool frame emitted")
	}
}

func TestSubscribesToSessionStart(t *testing.T) {
	// Adopting protocol 2 means subscribing to session_start so the host
	// delivers the event (events go only to subscribers).
	_, frames := runHandshake(t, `{"type":"hello_ack","zot_version":"0.103.2"}`)
	var events []string
	for _, f := range frames {
		if frameType(f) != "subscribe" {
			continue
		}
		_ = json.Unmarshal(f["events"], &events)
	}
	var subscribed bool
	for _, ev := range events {
		if ev == "session_start" {
			subscribed = true
		}
	}
	if !subscribed {
		t.Errorf("no subscribe frame for session_start (got events %v)", events)
	}
}

func TestSessionStartTracksLiveCWD(t *testing.T) {
	// On a protocol-2 host, session_start refreshes session identity and the
	// live cwd (which follows /cd); CWD() prefers it over the frozen launch cwd.
	e, _ := runHandshake(t,
		`{"type":"hello_ack","protocol_version":2,"zot_version":"0.104.0","terva_version":"0.104.0","cwd":"/launch/dir"}`,
		`{"type":"event","event":"session_start","session_id":"s1","session_title":"my session","cwd":"/work/now","project_id":"proj-abc"}`,
	)
	if got := e.Host().CWD; got != "/launch/dir" {
		t.Errorf("Host().CWD = %q, want the frozen launch cwd /launch/dir", got)
	}
	sess := e.Session()
	if sess.ID != "s1" || sess.ProjectID != "proj-abc" || sess.CWD != "/work/now" {
		t.Errorf("Session() = %+v, want id=s1 project=proj-abc cwd=/work/now", sess)
	}
	if got := e.CWD(); got != "/work/now" {
		t.Errorf("CWD() = %q, want the live session cwd /work/now", got)
	}
}

func TestCWDFallsBackToLaunchCWD(t *testing.T) {
	// Without a session_start (pre-v2 host, or no session yet), CWD() returns
	// the launch cwd from the handshake.
	e, _ := runHandshake(t, `{"type":"hello_ack","zot_version":"0.103.2","cwd":"/launch/dir"}`)
	if got := e.CWD(); got != "/launch/dir" {
		t.Errorf("CWD() = %q, want launch cwd /launch/dir", got)
	}
	if sess := e.Session(); sess != (Session{}) {
		t.Errorf("Session() = %+v, want zero value (no session_start fired)", sess)
	}
}

// Item 1: a tool_call dispatched through Run after a session_start must let the
// handler (which runs in its own goroutine) observe the live session cwd. The
// host guarantees session_start precedes the session's first tool_call, and Run
// processes frames in order, so the session is set before the tool goroutine
// reads it. Also the only direct coverage of tool_call dispatch via Run.
func TestToolCallObservesLiveSessionCWD(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"hello_ack","protocol_version":2,"terva_version":"0.104.0","cwd":"/launch"}`,
		`{"type":"event","event":"session_start","session_id":"s1","cwd":"/work/now"}`,
		`{"type":"tool_call","id":"t1","name":"probe","args":{}}`,
	}, "\n") + "\n"

	var buf bytes.Buffer
	e := &Extension{name: "web", in: strings.NewReader(in), out: &buf}
	seen := make(chan string, 1)
	e.Tool("probe", "probe", json.RawMessage(`{"type":"object"}`),
		func(json.RawMessage) Result {
			seen <- e.CWD()
			return Text("ok")
		})

	if err := e.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case got := <-seen:
		if got != "/work/now" {
			t.Errorf("handler observed CWD() = %q, want live session cwd /work/now", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool handler never ran")
	}
}

// Item 2: a second session_start (e.g. after /cd or a session switch) refreshes
// the live cwd.
func TestSessionStartRefireUpdatesCWD(t *testing.T) {
	e, _ := runHandshake(t,
		`{"type":"hello_ack","protocol_version":2,"terva_version":"0.104.0","cwd":"/launch"}`,
		`{"type":"event","event":"session_start","session_id":"s1","cwd":"/first"}`,
		`{"type":"event","event":"session_start","session_id":"s1","cwd":"/second"}`,
	)
	if got := e.CWD(); got != "/second" {
		t.Errorf("CWD() = %q after re-fire, want /second", got)
	}
}

// Item 3: a session_start that carries no cwd (e.g. --no-session) must not
// clobber the launch cwd — CWD() falls back to the handshake value.
func TestSessionStartEmptyCWDFallsBack(t *testing.T) {
	e, _ := runHandshake(t,
		`{"type":"hello_ack","terva_version":"0.104.0","cwd":"/launch"}`,
		`{"type":"event","event":"session_start","session_id":""}`,
	)
	if got := e.CWD(); got != "/launch" {
		t.Errorf("CWD() = %q, want fallback to launch cwd /launch", got)
	}
}

// Item 4: optimistic adoption means the hello must NOT declare a min_protocol,
// or an older (protocol-1) zot host would refuse to load the extension. Guards
// the backward-compatibility invariant against an accidental regression.
func TestHelloDeclaresNoMinProtocol(t *testing.T) {
	_, frames := runHandshake(t, `{"type":"hello_ack","zot_version":"0.103.2"}`)
	for _, f := range frames {
		if frameType(f) != "hello" {
			continue
		}
		if _, ok := f["min_protocol"]; ok {
			t.Error("hello frame carries min_protocol; want it absent (a floor would break pre-v2 hosts)")
		}
		return
	}
	t.Fatal("no hello frame emitted")
}

func TestSendImageResultNoCaption(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	e.sendToolResult("call-4", Image("image/jpeg", []byte{0xff, 0xd8, 0xff}, ""))

	f := parseResult(t, &buf)
	if len(f.Content) != 1 || f.Content[0].Type != "image" {
		t.Errorf("content = %+v, want a single image block (no empty caption)", f.Content)
	}
}
