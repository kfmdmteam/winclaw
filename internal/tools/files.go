package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func executeReadFile(input json.RawMessage) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("read_file: bad input: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("read_file: path is required")
	}

	data, err := os.ReadFile(params.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	content := string(data)
	const maxBytes = 20000
	if len(content) > maxBytes {
		content = content[:maxBytes] + fmt.Sprintf("\n... [truncated at %d bytes, file is %d bytes]", maxBytes, len(data))
	}
	return content, nil
}

func executeWriteFile(input json.RawMessage) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("write_file: bad input: %w", err)
	}
	if params.Path == "" {
		return "", fmt.Errorf("write_file: path is required")
	}

	if err := os.MkdirAll(filepath.Dir(params.Path), 0755); err != nil {
		return fmt.Sprintf("error creating directory: %v", err), nil
	}

	if err := os.WriteFile(params.Path, []byte(params.Content), 0644); err != nil {
		return fmt.Sprintf("error writing file: %v", err), nil
	}

	return fmt.Sprintf("wrote %d bytes to %s", len(params.Content), params.Path), nil
}

func executeListDir(input json.RawMessage) (string, error) {
	var params struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("list_directory: bad input: %w", err)
	}
	if params.Path == "" {
		params.Path = "."
	}

	entries, err := os.ReadDir(params.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Contents of %s:\n\n", params.Path))

	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		kind := "file"
		size := fmt.Sprintf("%8d", info.Size())
		if e.IsDir() {
			kind = "dir "
			size = "        "
		}
		sb.WriteString(fmt.Sprintf("%s  %s  %s  %s\n",
			kind,
			size,
			info.ModTime().Format(time.DateTime),
			e.Name(),
		))
	}
	return sb.String(), nil
}
