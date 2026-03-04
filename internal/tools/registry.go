package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"winclaw/internal/api"
)

// Registry holds all tools available to the agent.
type Registry struct {
	memUpdate  func(content string) error // callback to update session memory
	soulUpdate func(content string) error // callback to update soul file
}

// NewRegistry creates a tool registry with callbacks for memory/soul updates.
func NewRegistry(memUpdate, soulUpdate func(string) error) *Registry {
	return &Registry{memUpdate: memUpdate, soulUpdate: soulUpdate}
}

// Definitions returns the tool definitions to pass in each API request.
func (r *Registry) Definitions() []api.Tool {
	return []api.Tool{
		{
			Name:        "bash",
			Description: "Run a PowerShell command on this Windows machine and return its output. Use for system tasks, file operations via shell, network diagnostics, etc. Always prefer specific commands over broad ones. Ask the user before running anything destructive.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"command": {
						"type": "string",
						"description": "PowerShell command to execute"
					},
					"timeout_seconds": {
						"type": "integer",
						"description": "Timeout in seconds. Default 30, max 120."
					}
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
			Description: "Search the web using DuckDuckGo and return a summary of results. Use for current information, documentation lookups, and anything requiring up-to-date knowledge.",
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
			Description: "Fetch the text content of a URL. Returns the page content with HTML stripped. Use for reading documentation, articles, or any specific web page.",
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
			Description: "Append important information to your persistent session memory. This is injected into your system prompt on every turn. Use this to remember facts about the user, their projects, preferences, or anything worth keeping. Write concise, factual bullet points.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"content": {"type": "string", "description": "Content to append to memory"}
				},
				"required": ["content"]
			}`),
		},
		{
			Name:        "update_soul",
			Description: "Rewrite your soul file — your persistent identity, values, and self-knowledge. Update this when you learn something fundamental about yourself or your purpose. The soul file is injected into every system prompt.",
			InputSchema: mustJSON(`{
				"type": "object",
				"properties": {
					"content": {"type": "string", "description": "Full new content for the soul file"}
				},
				"required": ["content"]
			}`),
		},
	}
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
	default:
		return "", fmt.Errorf("unknown tool: %q", name)
	}
}

func mustJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}
