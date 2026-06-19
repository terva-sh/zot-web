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

// ProtocolVersion is the newest wire revision whose frames this package
// handles, tracking terva's extproto.ProtocolVersion:
//
//	1 — baseline: tool_result fanout, crash surfacing, min-protocol negotiation.
//	2 — session identity: a session_start event carrying session_id/path/title
//	    plus a live cwd/project_id that follow /cd and session switches.
//
// An extension announces no protocol version on the wire: the host's
// protocol_version arrives in hello_ack, and the extension's only lever is an
// optional min_protocol floor in its hello. We deliberately send NO
// min_protocol, so an older (protocol-1) zot host still loads this extension;
// protocol 2's additions are adopted opportunistically, not required. This
// constant is a local yardstick only — it gates the drift note below (we log a
// host NEWER than this, and degrade against older ones by simply not receiving
// the v2 frames).
const ProtocolVersion = 2

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
	authority   string
}

// ToolOption configures a tool at registration time. Pass options as trailing
// arguments to Tool.
type ToolOption func(*toolDef)

// WithAuthority declares a tool's authority class, mirroring terva's
// ext.WithAuthority. terva uses it to gate the tool (only "local-read" is
// auto-allowable; everything else prompts or is refused per mode). The field is
// sent on register_tool; upstream zot hosts ignore the unknown field, so it is
// safe to set unconditionally.
func WithAuthority(class string) ToolOption {
	return func(t *toolDef) { t.authority = class }
}

// NetworkRead marks a tool as one that reads from the network — the correct
// class for fetch/search tools. On a terva host this makes the tool prompt in
// workspace/auto-edit and be refused in plan, rather than auto-allowed.
func NetworkRead() ToolOption { return WithAuthority("network-read") }

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
	TervaVersion    string
	DataDir         string
	ExtensionDir    string
	Provider        string
	Model           string
	CWD             string
}

// IsTerva reports whether the host is terva rather than upstream zot. terva is
// a hard fork of zot that keeps zot's extension wire protocol; its hello_ack
// adds a terva_version field (sent only by terva) while still sending
// zot_version so plain zot extensions keep working. That added field's
// presence — not a version comparison — is the robust zot-vs-terva
// discriminator.
func (h Host) IsTerva() bool { return h.TervaVersion != "" }

// Session carries the active-session identity a protocol-2 host sends on a
// session_start event. It is empty until such an event arrives: a pre-v2 zot
// host never fires one, and even on terva there is no session under
// --no-session. Unlike Host (frozen at the handshake), these fields refresh on
// every session_start — a session switch (/sessions resume, fork, /new) or a
// /cd — so CWD tracks the live working directory instead of the launch cwd.
// ProjectID is the host's stable, collision-proof key for CWD, for scoping
// per-project state.
type Session struct {
	ID        string
	Path      string
	Title     string
	CWD       string
	ProjectID string
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
	session  Session
}

// New constructs an Extension that talks to zot over stdin/stdout.
func New(name, version string) *Extension {
	return &Extension{name: name, version: version, in: os.Stdin, out: os.Stdout}
}

// Tool registers an LLM-callable tool. Call before Run. schema is a JSON Schema
// object (same shape Anthropic/OpenAI accept).
func (e *Extension) Tool(name, description string, schema json.RawMessage, h ToolHandler, opts ...ToolOption) {
	td := toolDef{name: name, description: description, schema: schema, handler: h}
	for _, opt := range opts {
		opt(&td)
	}
	e.mu.Lock()
	e.tools = append(e.tools, td)
	e.mu.Unlock()
}

// ToolInfo is metadata for a registered tool, returned by Tools for
// introspection and testing (e.g. asserting every network tool declares its
// authority).
type ToolInfo struct {
	Name        string
	Description string
	Authority   string
}

// Tools returns metadata for the registered tools, in registration order.
func (e *Extension) Tools() []ToolInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]ToolInfo, len(e.tools))
	for i, t := range e.tools {
		out[i] = ToolInfo{Name: t.name, Description: t.description, Authority: t.authority}
	}
	return out
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

// Session returns the active session a protocol-2 host last reported via
// session_start. Zero value on a pre-v2 host, or before the first session opens
// (the host guarantees session_start arrives before that session's first
// tool_call, so a tool handler sees the current session).
func (e *Extension) Session() Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.session
}

// CWD returns the working directory to resolve workspace-relative paths
// against: the live session cwd when a protocol-2 host has sent one (it follows
// /cd), else the launch cwd frozen in the hello_ack. Tool handlers that write
// files should use this rather than Host().CWD so saves land in the user's
// current directory after a /cd.
func (e *Extension) CWD() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session.CWD != "" {
		return e.session.CWD
	}
	return e.host.CWD
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
	caps := []string{"tools", "events"}
	if len(commands) > 0 {
		caps = append(caps, "commands")
	}
	e.send(map[string]any{
		"type": "hello", "name": e.name, "version": e.version,
		"capabilities": caps,
	})
	for _, t := range tools {
		frame := map[string]any{
			"type": "register_tool", "name": t.name,
			"description": t.description, "schema": t.schema,
		}
		// Sent unconditionally: registration happens before hello_ack arrives,
		// so the host's identity isn't known yet. terva consumes authority;
		// upstream zot hosts ignore the unknown field harmlessly.
		if t.authority != "" {
			frame["authority"] = t.authority
		}
		e.send(frame)
	}
	for _, c := range commands {
		e.send(map[string]any{
			"type": "register_command", "name": c.name,
			"description": c.description,
		})
	}
	// Subscribe to session_start (protocol 2). Sent during the register phase so
	// we are subscribed before the host's ordered-delivery guarantee kicks in
	// (session_start reaches a subscriber before that session's first
	// tool_call). A pre-v2 host simply records the subscription and never fires
	// the event — harmless.
	e.send(map[string]any{"type": "subscribe", "events": []string{"session_start"}})
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
			TervaVersion    string          `json:"terva_version"`
			DataDir         string          `json:"data_dir"`
			ExtensionDir    string          `json:"extension_dir"`
			Provider        string          `json:"provider"`
			Model           string          `json:"model"`
			CWD             string          `json:"cwd"`

			// Lifecycle event fields (type:"event"). For session_start the
			// host also resends cwd (decoded above), which refreshes on /cd.
			Event        string `json:"event"`
			SessionID    string `json:"session_id"`
			SessionPath  string `json:"session_path"`
			SessionTitle string `json:"session_title"`
			ProjectID    string `json:"project_id"`
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
				TervaVersion:    f.TervaVersion,
				DataDir:         f.DataDir,
				ExtensionDir:    f.ExtensionDir,
				Provider:        f.Provider,
				Model:           f.Model,
				CWD:             f.CWD,
			}
			e.mu.Unlock()
			if f.TervaVersion != "" {
				e.Logf("host identity: terva %s (zot-compat %s), protocol_version %d", f.TervaVersion, f.ZotVersion, f.ProtocolVersion)
			} else {
				e.Logf("host identity: zot %s, protocol_version %d", f.ZotVersion, f.ProtocolVersion)
			}
			if f.ProtocolVersion > ProtocolVersion {
				e.Logf("note: host speaks protocol_version %d, newer than this extension's %d; newer host features are ignored, everything else works",
					f.ProtocolVersion, ProtocolVersion)
			}
		case "event":
			// One-way lifecycle events; we subscribe only to session_start.
			if f.Event == "session_start" {
				e.mu.Lock()
				e.session = Session{
					ID:        f.SessionID,
					Path:      f.SessionPath,
					Title:     f.SessionTitle,
					CWD:       f.CWD,
					ProjectID: f.ProjectID,
				}
				e.mu.Unlock()
				e.Logf("session_start: id=%q project=%q cwd=%q", f.SessionID, f.ProjectID, f.CWD)
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
