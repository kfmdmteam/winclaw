package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"winclaw/internal/api"
)

// Options contains optional callbacks and configuration for the Registry.
type Options struct {
	// GlobalUpdate is called by the update_global_memory tool.
	GlobalUpdate func(string) error
	// DelegateFunc is called by the delegate tool to run a sub-agent.
	DelegateFunc func(ctx context.Context, prompt string) (string, error)
	// PluginDir is scanned for *.ps1 plugin tools on startup.
	PluginDir string
}

// Registry holds all tools available to the agent.
type Registry struct {
	memUpdate  func(string) error
	soulUpdate func(string) error
	opts       Options
	plugins    []pluginDef
}

// NewRegistry creates a tool registry.
// memUpdate/soulUpdate are required callbacks for the built-in memory tools.
// opts provides optional capabilities: global memory, delegation, and plugins.
func NewRegistry(memUpdate, soulUpdate func(string) error, opts Options) *Registry {
	r := &Registry{
		memUpdate:  memUpdate,
		soulUpdate: soulUpdate,
		opts:       opts,
	}
	if opts.PluginDir != "" {
		r.loadPlugins(opts.PluginDir)
	}
	return r
}

// Definitions returns the tool definitions to pass in each API request.
func (r *Registry) Definitions() []api.Tool {
	defs := []api.Tool{
		{
			Name:        "bash",
			Description: "Run a PowerShell command on this Windows machine and return its output. Use for system tasks, file operations, network diagnostics, etc. Ask the user before running anything destructive.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "PowerShell command to execute"},
					"timeout_seconds": {"type": "integer", "description": "Timeout in seconds. Default 30, max 120."}
				},
				"required": ["command"]
			}`),
		},
		{
			Name:        "read_file",
			Description: "Read the full contents of a file on the Windows filesystem.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Absolute or relative file path"}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "write_file",
			Description: "Write content to a file. Creates the file if it does not exist, overwrites if it does. Ask the user before overwriting important files.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"path":    {"type": "string", "description": "File path to write"},
					"content": {"type": "string", "description": "Content to write"}
				},
				"required": ["path", "content"]
			}`),
		},
		{
			Name:        "list_directory",
			Description: "List the contents of a directory, showing file names, sizes, and modification times.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Directory path to list"}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "web_search",
			Description: "Search the web using DuckDuckGo and return a summary of results.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"query": {"type": "string", "description": "Search query"}
				},
				"required": ["query"]
			}`),
		},
		{
			Name:        "fetch_url",
			Description: "Fetch the text content of a URL. Returns the page content with HTML stripped.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"url": {"type": "string", "description": "URL to fetch"}
				},
				"required": ["url"]
			}`),
		},
		{
			Name:        "update_memory",
			Description: "Append important information to your persistent session memory. This is injected into your system prompt on every turn. Write concise, factual bullet points.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"content": {"type": "string", "description": "Content to append to session memory"}
				},
				"required": ["content"]
			}`),
		},
		{
			Name:        "update_soul",
			Description: "Rewrite your soul file — your persistent identity, values, and self-knowledge. The soul file is injected into every system prompt.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"content": {"type": "string", "description": "Full new content for the soul file"}
				},
				"required": ["content"]
			}`),
		},
		// ── Windows-native tools ──────────────────────────────────────────────
		{
			Name:        "screenshot",
			Description: "Capture the primary monitor as a PNG image and return it for visual analysis. Use when the user asks what is on screen, to read UI text, or inspect desktop state.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"max_width": {"type": "integer", "description": "Maximum image width in pixels. Default 1280."}
				}
			}`),
		},
		{
			Name:        "process_list",
			Description: "List running processes. Returns name, PID, CPU time, and memory usage for the top 50 processes (or filtered by name).",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"filter": {"type": "string", "description": "Optional partial name filter"}
				}
			}`),
		},
		{
			Name:        "kill_process",
			Description: "Terminate a process by PID or name. Ask the user before killing critical system processes.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"pid":  {"type": "integer", "description": "Process ID to kill"},
					"name": {"type": "string", "description": "Process name to kill (kills all matching)"}
				}
			}`),
		},
		{
			Name:        "toast_notify",
			Description: "Send a Windows desktop notification (balloon tip). Useful for alerting the user when a background task completes.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"title":   {"type": "string", "description": "Notification title (default: WinClaw)"},
					"message": {"type": "string", "description": "Notification body text"}
				},
				"required": ["message"]
			}`),
		},
		{
			Name:        "run_elevated",
			Description: "Run a PowerShell command with administrator privileges via UAC elevation. Use only when elevation is genuinely required.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "PowerShell command to run as administrator"}
				},
				"required": ["command"]
			}`),
		},
		{
			Name:        "registry_read",
			Description: "Read a Windows registry value or key. Use PowerShell-style paths: HKCU:\\Software\\... or HKLM:\\Software\\...",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Registry key path (e.g. HKCU:\\Software\\MyApp)"},
					"name": {"type": "string", "description": "Value name (omit to read all values in the key)"}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "registry_write",
			Description: "Write a Windows registry value. Ask the user before modifying system keys.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"path":  {"type": "string", "description": "Registry key path"},
					"name":  {"type": "string", "description": "Value name"},
					"value": {"type": "string", "description": "Value data"},
					"type":  {"type": "string", "description": "Registry type: String (default), DWord, QWord, Binary, ExpandString, MultiString"}
				},
				"required": ["path", "name", "value"]
			}`),
		},
	}

	// Optional: global memory tool.
	if r.opts.GlobalUpdate != nil {
		defs = append(defs, api.Tool{
			Name:        "update_global_memory",
			Description: "Append important facts to the cross-session global memory. This is injected into every session's system prompt, making it available everywhere across all sessions. Use for user preferences and facts that should persist globally.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"content": {"type": "string", "description": "Content to append to global memory"}
				},
				"required": ["content"]
			}`),
		})
	}

	// Optional: delegate tool.
	if r.opts.DelegateFunc != nil {
		defs = append(defs, api.Tool{
			Name:        "delegate",
			Description: "Delegate a sub-task to a fresh AI agent with no prior conversation history. The sub-agent has all the same tools. Use for isolated parallel sub-tasks that benefit from a clean context.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"prompt":  {"type": "string", "description": "Complete task description for the sub-agent"},
					"context": {"type": "string", "description": "Optional extra context to prepend to the prompt"}
				},
				"required": ["prompt"]
			}`),
		})
	}

	// Plugins loaded from the plugin directory.
	for _, p := range r.plugins {
		defs = append(defs, api.Tool{
			Name:        p.Name,
			Description: p.Description,
			InputSchema: p.Parameters,
		})
	}

	return defs
}

// Execute runs the named tool with the given JSON input and returns the result.
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	switch name {
	case "bash":
		return executeBash(ctx, input)
	case "read_file":
		return executeReadFile(input)
	case "write_file":
		return executeWriteFile(input)
	case "list_directory":
		return executeListDir(input)
	case "web_search":
		return executeWebSearch(ctx, input)
	case "fetch_url":
		return executeFetchURL(ctx, input)
	case "update_memory":
		return executeUpdateMemory(input, r.memUpdate)
	case "update_soul":
		return executeUpdateSoul(input, r.soulUpdate)
	case "screenshot":
		return executeScreenshot(ctx, input)
	case "process_list":
		return executeProcessList(ctx, input)
	case "kill_process":
		return executeKillProcess(ctx, input)
	case "toast_notify":
		return executeToastNotify(ctx, input)
	case "run_elevated":
		return executeRunElevated(ctx, input)
	case "registry_read":
		return executeRegistryRead(ctx, input)
	case "registry_write":
		return executeRegistryWrite(ctx, input)
	case "update_global_memory":
		if r.opts.GlobalUpdate == nil {
			return "", fmt.Errorf("global memory not configured")
		}
		return executeUpdateGlobalMemory(input, r.opts.GlobalUpdate)
	case "delegate":
		if r.opts.DelegateFunc == nil {
			return "", fmt.Errorf("delegate not configured")
		}
		return executeDelegate(ctx, input, r.opts.DelegateFunc)
	default:
		for _, p := range r.plugins {
			if p.Name == name {
				return r.executePlugin(ctx, p, input)
			}
		}
		return "", fmt.Errorf("unknown tool: %q", name)
	}
}

func mustJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}

// executeUpdateGlobalMemory appends content to the global memory file.
func executeUpdateGlobalMemory(input json.RawMessage, update func(string) error) (string, error) {
	var args struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("update_global_memory: %w", err)
	}
	if args.Content == "" {
		return "", fmt.Errorf("update_global_memory: content must not be empty")
	}
	if err := update(args.Content); err != nil {
		return "", fmt.Errorf("update_global_memory: %w", err)
	}
	return "Global memory updated.", nil
}

// executeDelegate runs a sub-task via the delegateFunc callback.
func executeDelegate(ctx context.Context, input json.RawMessage, fn func(context.Context, string) (string, error)) (string, error) {
	var args struct {
		Prompt  string `json:"prompt"`
		Context string `json:"context"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("delegate: %w", err)
	}
	if args.Prompt == "" {
		return "", fmt.Errorf("delegate: prompt is required")
	}
	full := args.Prompt
	if args.Context != "" {
		full = args.Context + "\n\n" + args.Prompt
	}
	return fn(ctx, full)
}
