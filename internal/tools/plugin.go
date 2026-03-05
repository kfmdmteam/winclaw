//go:build windows

package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pluginDef describes a single plugin loaded from a .ps1 file.
type pluginDef struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	ScriptPath  string
}

// loadPlugins scans dir for *.ps1 files with a WinClaw-Plugin header and
// appends valid plugins to r.plugins. Errors loading individual plugins are
// silently skipped.
func (r *Registry) loadPlugins(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".ps1") {
			continue
		}
		p, err := parsePlugin(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		r.plugins = append(r.plugins, p)
	}
}

// parsePlugin reads the header comment block from a .ps1 file and extracts
// the plugin metadata. The expected format is:
//
//	# WinClaw-Plugin
//	# Name: tool_name
//	# Description: Human-readable description.
//	# Parameters: {"type":"object","properties":{...},"required":[...]}
func parsePlugin(path string) (pluginDef, error) {
	f, err := os.Open(path)
	if err != nil {
		return pluginDef{}, err
	}
	defer f.Close()

	var (
		isPlugin    bool
		name        string
		description string
		parameters  string
	)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "#") {
			break
		}
		line = strings.TrimPrefix(line, "#")
		line = strings.TrimSpace(line)

		switch {
		case line == "WinClaw-Plugin":
			isPlugin = true
		case strings.HasPrefix(line, "Name:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		case strings.HasPrefix(line, "Description:"):
			description = strings.TrimSpace(strings.TrimPrefix(line, "Description:"))
		case strings.HasPrefix(line, "Parameters:"):
			parameters = strings.TrimSpace(strings.TrimPrefix(line, "Parameters:"))
		}
	}

	if !isPlugin || name == "" {
		return pluginDef{}, fmt.Errorf("not a WinClaw plugin or missing Name header")
	}

	params := json.RawMessage(`{"type":"object","properties":{}}`)
	if parameters != "" {
		params = json.RawMessage(parameters)
	}

	return pluginDef{
		Name:        name,
		Description: description,
		Parameters:  params,
		ScriptPath:  path,
	}, nil
}

// executePlugin runs a plugin .ps1 file, passing the tool input JSON via the
// -InputJson parameter. Returns stdout output trimmed to 16 KB.
func (r *Registry) executePlugin(ctx context.Context, p pluginDef, input json.RawMessage) (string, error) {
	raw := string(input)
	if raw == "" || raw == "null" {
		raw = "{}"
	}
	// Escape single quotes for embedding in the PowerShell argument.
	escaped := strings.ReplaceAll(raw, "'", "''")
	ps := fmt.Sprintf(`& '%s' -InputJson '%s'`, p.ScriptPath, escaped)
	return executePowerShell(ctx, ps, 30)
}
