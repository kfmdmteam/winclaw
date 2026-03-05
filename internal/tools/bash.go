package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// executePowerShell runs a raw PowerShell script string with the given timeout.
// Used internally by Windows-native tool functions.
func executePowerShell(ctx context.Context, script string, timeoutSeconds int) (string, error) {
	b, _ := json.Marshal(script)
	raw := json.RawMessage(fmt.Sprintf(`{"command":%s,"timeout_seconds":%d}`, string(b), timeoutSeconds))
	return executeBash(ctx, raw)
}

func executeBash(ctx context.Context, input json.RawMessage) (string, error) {
	var params struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return "", fmt.Errorf("bash: bad input: %w", err)
	}
	if params.TimeoutSeconds <= 0 || params.TimeoutSeconds > 120 {
		params.TimeoutSeconds = 30
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx,
		"powershell",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", params.Command,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var sb strings.Builder
	if stdout.Len() > 0 {
		sb.WriteString(stdout.String())
	}
	if stderr.Len() > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("[stderr]\n")
		sb.WriteString(stderr.String())
	}
	if runErr != nil {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("[exit error] %v", runErr))
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		result = "(no output)"
	}

	// Cap output to prevent token explosion.
	const maxOutput = 8000
	if len(result) > maxOutput {
		result = result[:maxOutput] + fmt.Sprintf("\n... [truncated, %d total chars]", len(result))
	}

	return result, nil // errors are returned in content, not as Go errors
}
