// Package proto implements the zot extension wire protocol (newline-delimited
// JSON over stdio) directly, so this extension depends on nothing but the Go
// standard library — no coupling to the zot module or its version.
//
// It implements the subset a tool-providing extension needs: the hello
// handshake, register_tool, the ready sentinel, tool_call dispatch,
// tool_result replies, and graceful shutdown. See docs/extensions.md in the
// zot repo for the full protocol. Field names here mirror zot's extproto
// package exactly (e.g. is_error, mime_type, snake_case throughout).
package proto

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// ProtocolVersion is the zot extension protocol version this package speaks. It
// is sent by the host in hello_ack; a mismatch is logged (not fatal) so the
// extension keeps working against minor host changes while surfacing a drift.
const ProtocolVersion = 1

// Result is a tool handler's reply. Text is sent back to the model as a text
// content block; Image, when set, is sent as an image content block (before the
// text) so the host injects it into the model context as a real image rather
// than base64 text. IsError marks the call as failed.
type Result struct {
	Text    string
	IsError bool
	Image   *ImageContent
}

// ImageContent is a binary image returned to the model for native multimodal
// injection. Data is the raw encoded image (PNG/JPEG/GIF/WebP/…); the wire
// layer base64-encodes it. MimeType is its media type (e.g. "image/png").
type ImageContent struct {
	MimeType string
	Data     []byte
}

// Text builds a successful Result.
func Text(s string) Result { return Result{Text: s} }

// Image builds a successful Result carrying a native image block plus an
// optional caption (sent as a trailing text block when non-empty).
func Image(mimeType string, data []byte, caption string) Result {
	return Result{Text: caption, Image: &ImageContent{MimeType: mimeType, Data: data}}
}

// Errorf builds a failed Result with a formatted message.
func Errorf(format string, a ...any) Result {
	return Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

// ToolHandler runs when the model invokes a registered tool. args is the raw
// JSON object the model produced; the handler validates it.
type ToolHandler func(args json.RawMessage) Result

type toolDef struct {
	name        string
	description string
	schema      json.RawMessage
	handler     ToolHandler
}

// CommandResult is a slash-command handler's reply. Action selects how zot
// renders Text: "display" (one-shot styled note in the chat), "prompt"
// (submit Text as a user message), "insert" (insert into the editor), or
// "noop" (the handler already did its work, e.g. via Notify). A non-empty
// Err renders as a red status line.
type CommandResult struct {
	Action string
	Text   string
	Err    string
}

// Display builds a CommandResult that shows s as a one-shot chat note.
func Display(s string) CommandResult { return CommandResult{Action: "display", Text: s} }

// CommandHandler runs when the user invokes a registered slash command. args
// is everything typed after the command name, trimmed.
type CommandHandler func(args string) CommandResult

type commandDef struct {
	name        string
	description string
	handler     CommandHandler
}

// Host carries the hello_ack fields this extension cares about.
type Host struct {
	ProtocolVersion int
	ZotVersion      string
	DataDir         string
	ExtensionDir    string
	Provider        string
	Model           string
	CWD             string
}

// Extension is one tool-providing extension. Construct with New, register
// tools, then call Run.
type Extension struct {
	name    string
	version string

	in      io.Reader
	out     io.Writer
	writeMu sync.Mutex

	mu       sync.Mutex
	tools    []toolDef
	commands []commandDef
	host     Host
}

// New constructs an Extension that talks to zot over stdin/stdout.
func New(name, version string) *Extension {
	return &Extension{name: name, version: version, in: os.Stdin, out: os.Stdout}
}

// Tool registers an LLM-callable tool. Call before Run. schema is a JSON Schema
// object (same shape Anthropic/OpenAI accept).
func (e *Extension) Tool(name, description string, schema json.RawMessage, h ToolHandler) {
	e.mu.Lock()
	e.tools = append(e.tools, toolDef{name, description, schema, h})
	e.mu.Unlock()
}

// Command registers a user-invocable slash command. Call before Run.
func (e *Extension) Command(name, description string, h CommandHandler) {
	e.mu.Lock()
	e.commands = append(e.commands, commandDef{name, description, h})
	e.mu.Unlock()
}

// Notify pushes a one-shot status note below the transcript (cleared on the
// user's next prompt). level is "info", "success", "warn", or "error".
func (e *Extension) Notify(level, format string, a ...any) {
	e.send(map[string]any{
		"type": "notify", "level": level,
		"message": fmt.Sprintf(format, a...),
	})
}

// Host returns the info zot sent in hello_ack. Zero value until the handshake
// completes (which happens before any tool_call).
func (e *Extension) Host() Host {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.host
}

// Logf writes a debug line to stderr, which zot captures to
// $ZOT_HOME/logs/ext-<name>.log. Never write to stdout — that's the wire.
func (e *Extension) Logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "["+e.name+"] "+format+"\n", a...)
}

func (e *Extension) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	_, _ = e.out.Write(append(b, '\n'))
}

// Run sends the hello + registrations, then serves tool calls until zot closes
// stdin or sends shutdown. Blocks until then.
func (e *Extension) Run() error {
	e.mu.Lock()
	tools := append([]toolDef(nil), e.tools...)
	commands := append([]commandDef(nil), e.commands...)
	e.mu.Unlock()
	caps := []string{"tools"}
	if len(commands) > 0 {
		caps = append(caps, "commands")
	}
	e.send(map[string]any{
		"type": "hello", "name": e.name, "version": e.version,
		"capabilities": caps,
	})
	for _, t := range tools {
		e.send(map[string]any{
			"type": "register_tool", "name": t.name,
			"description": t.description, "schema": t.schema,
		})
	}
	for _, c := range commands {
		e.send(map[string]any{
			"type": "register_command", "name": c.name,
			"description": c.description,
		})
	}
	e.send(map[string]any{"type": "ready"})

	sc := bufio.NewScanner(e.in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var f struct {
			Type            string          `json:"type"`
			ID              string          `json:"id"`
			Name            string          `json:"name"`
			Args            json.RawMessage `json:"args"`
			ProtocolVersion int             `json:"protocol_version"`
			ZotVersion      string          `json:"zot_version"`
			DataDir         string          `json:"data_dir"`
			ExtensionDir    string          `json:"extension_dir"`
			Provider        string          `json:"provider"`
			Model           string          `json:"model"`
			CWD             string          `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			e.Logf("bad frame from host: %v", err)
			continue
		}
		switch f.Type {
		case "hello_ack":
			e.mu.Lock()
			e.host = Host{
				ProtocolVersion: f.ProtocolVersion,
				ZotVersion:      f.ZotVersion,
				DataDir:         f.DataDir,
				ExtensionDir:    f.ExtensionDir,
				Provider:        f.Provider,
				Model:           f.Model,
				CWD:             f.CWD,
			}
			e.mu.Unlock()
			if f.ProtocolVersion != 0 && f.ProtocolVersion != ProtocolVersion {
				e.Logf("warning: host speaks protocol_version %d but this extension implements %d (zot %s); proceeding, but behavior may drift",
					f.ProtocolVersion, ProtocolVersion, f.ZotVersion)
			}
		case "tool_call":
			h := e.handlerFor(f.Name)
			if h == nil {
				e.sendToolResult(f.ID, Errorf("no handler for tool %q", f.Name))
				continue
			}
			// Own goroutine so a slow fetch doesn't block other calls.
			go func(id string, args json.RawMessage) {
				defer func() {
					if r := recover(); r != nil {
						e.sendToolResult(id, Errorf("panic: %v", r))
					}
				}()
				e.sendToolResult(id, h(args))
			}(f.ID, f.Args)
		case "command_invoked":
			// args is a plain string for commands (everything after the name).
			var cmdArgs string
			_ = json.Unmarshal(f.Args, &cmdArgs)
			h := e.commandFor(f.Name)
			if h == nil {
				e.sendCommandResult(f.ID, CommandResult{Action: "noop", Err: fmt.Sprintf("no handler for command %q", f.Name)})
				continue
			}
			go func(id, args string) {
				defer func() {
					if r := recover(); r != nil {
						e.sendCommandResult(id, CommandResult{Action: "noop", Err: fmt.Sprintf("panic: %v", r)})
					}
				}()
				e.sendCommandResult(id, h(args))
			}(f.ID, cmdArgs)
		case "shutdown":
			e.send(map[string]any{"type": "shutdown_ack"})
			return nil
		}
	}
	return sc.Err()
}

func (e *Extension) handlerFor(name string) ToolHandler {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.tools {
		if t.name == name {
			return t.handler
		}
	}
	return nil
}

func (e *Extension) commandFor(name string) CommandHandler {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.commands {
		if c.name == name {
			return c.handler
		}
	}
	return nil
}

func (e *Extension) sendCommandResult(id string, r CommandResult) {
	action := r.Action
	if action == "" {
		action = "noop"
	}
	frame := map[string]any{"type": "command_response", "id": id, "action": action}
	switch action {
	case "display", "prompt", "insert":
		frame[action] = r.Text
	}
	if r.Err != "" {
		frame["error"] = r.Err
	}
	e.send(frame)
}

func (e *Extension) sendToolResult(id string, r Result) {
	content := make([]map[string]any, 0, 2)
	if r.Image != nil {
		content = append(content, map[string]any{
			"type":      "image",
			"mime_type": r.Image.MimeType,
			"data":      base64.StdEncoding.EncodeToString(r.Image.Data),
		})
	}
	// Always carry a text block when there's no image (preserving prior
	// behavior); with an image, add one only for a non-empty caption.
	if r.Text != "" || len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": r.Text})
	}
	e.send(map[string]any{
		"type":     "tool_result",
		"id":       id,
		"content":  content,
		"is_error": r.IsError,
	})
}
