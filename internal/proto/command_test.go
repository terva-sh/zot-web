package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSendCommandResultDisplay(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	e.sendCommandResult("cmd-1", Display("hello note"))

	var f struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Action  string `json:"action"`
		Display string `json:"display"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Type != "command_response" || f.ID != "cmd-1" || f.Action != "display" || f.Display != "hello note" {
		t.Errorf("frame = %+v", f)
	}
}

func TestNotifyFrame(t *testing.T) {
	var buf bytes.Buffer
	e := &Extension{out: &buf}
	e.Notify("warn", "rate limit %s", "hit")

	var f struct {
		Type    string `json:"type"`
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if f.Type != "notify" || f.Level != "warn" || f.Message != "rate limit hit" {
		t.Errorf("frame = %+v", f)
	}
}

// TestRunDispatchesCommand drives the full Run loop with a command_invoked
// frame and asserts the hello advertises commands, the command is registered,
// and the handler's response is written.
func TestRunDispatchesCommand(t *testing.T) {
	in := `{"type":"hello_ack","protocol_version":1}` + "\n" +
		`{"type":"command_invoked","id":"c1","name":"web-cache","args":"clear"}` + "\n"
	var buf bytes.Buffer
	e := &Extension{name: "web", version: "test", in: strings.NewReader(in), out: &buf}
	e.Command("web-cache", "inspect the cache", func(args string) CommandResult {
		return Display("got args: " + args)
	})
	if err := e.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	// The handler runs in its own goroutine; poll (under the write lock) for
	// its response.
	deadline := time.Now().Add(2 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		e.writeMu.Lock()
		out = buf.String()
		e.writeMu.Unlock()
		if strings.Contains(out, "command_response") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, want := range []string{
		`"capabilities":["tools","events","commands"]`,
		`"type":"register_command"`,
		`"name":"web-cache"`,
		`"type":"command_response"`,
		`"got args: clear"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %s:\n%s", want, out)
		}
	}
}
