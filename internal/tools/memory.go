package tools

import (
	"encoding/json"
	"fmt"
)

func executeUpdateMemory(input json.RawMessage, update func(string) error) (string, error) {
	var params struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("update_memory: bad input: %w", err)
	}
	if params.Content == "" {
		return "nothing to save", nil
	}
	if err := update(params.Content); err != nil {
		return fmt.Sprintf("failed to update memory: %v", err), nil
	}
	return "memory updated", nil
}

func executeUpdateSoul(input json.RawMessage, update func(string) error) (string, error) {
	var params struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("update_soul: bad input: %w", err)
	}
	if params.Content == "" {
		return "nothing to save", nil
	}
	if err := update(params.Content); err != nil {
		return fmt.Sprintf("failed to update soul: %v", err), nil
	}
	return "soul updated", nil
}
