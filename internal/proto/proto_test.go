package proto

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
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

func TestSendImageResultNoCaption(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	e.sendToolResult("call-4", Image("image/jpeg", []byte{0xff, 0xd8, 0xff}, ""))

	f := parseResult(t, &buf)
	if len(f.Content) != 1 || f.Content[0].Type != "image" {
		t.Errorf("content = %+v, want a single image block (no empty caption)", f.Content)
	}
}

// registerFrames runs the extension against a stdin that immediately
// shuts it down, then returns the register_tool frames it emitted.
func registerFrames(t *testing.T, e *Extension) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	e.out = &out
	e.in = bytes.NewBufferString(`{"type":"shutdown"}` + "\n")
	if err := e.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var frames []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var f map[string]any
		if json.Unmarshal(line, &f) == nil && f["type"] == "register_tool" {
			frames = append(frames, f)
		}
	}
	return frames
}

// TestRegisterToolReadOnly pins the backwards-compatibility contract:
// a ReadOnly() tool emits "read_only":true, an un-annotated tool emits
// no read_only key at all (so the frame is byte-for-byte what stock
// zot has always received).
func TestRegisterToolReadOnly(t *testing.T) {
	e := New("web", "test")
	e.Tool("web_search", "search", json.RawMessage(`{"type":"object"}`), func(json.RawMessage) Result { return Text("") }, ReadOnly())
	e.Tool("web_fetch_raw", "save", json.RawMessage(`{"type":"object"}`), func(json.RawMessage) Result { return Text("") })

	frames := registerFrames(t, e)
	got := map[string]map[string]any{}
	for _, f := range frames {
		got[f["name"].(string)] = f
	}
	if len(got) != 2 {
		t.Fatalf("want 2 register_tool frames, got %d", len(got))
	}
	if ro, ok := got["web_search"]["read_only"]; !ok || ro != true {
		t.Errorf("read-only tool frame = %v, want read_only:true", got["web_search"])
	}
	if _, present := got["web_fetch_raw"]["read_only"]; present {
		t.Errorf("un-annotated tool must omit read_only entirely (zot wire compat), got %v", got["web_fetch_raw"])
	}
}
