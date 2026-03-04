package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/memory"
	"winclaw/internal/tools"
)

const (
	defaultMaxTurns    = 20
	defaultMaxToolLoop = 10 // max tool-call iterations per user turn
)

// Agent drives a single session's conversation with the Anthropic API.
type Agent struct {
	Session    *Session
	Client     *api.Client
	Memory     *memory.MemoryManager
	Config     *config.Config
	Tools      *tools.Registry
	MaxTurns   int
	TokensUsed int
	onOutput   func(text string)
}

// NewAgent constructs an Agent. onOutput may be nil.
func NewAgent(
	sess *Session,
	client *api.Client,
	mem *memory.MemoryManager,
	cfg *config.Config,
	tl *tools.Registry,
	onOutput func(string),
) *Agent {
	return &Agent{
		Session:  sess,
		Client:   client,
		Memory:   mem,
		Config:   cfg,
		Tools:    tl,
		MaxTurns: defaultMaxTurns,
		onOutput: onOutput,
	}
}

// Run processes a single user input through the agentic loop:
//  1. Send messages + tools to the API.
//  2. If the model calls tools, execute them and send results back.
//  3. Repeat until stop_reason is "end_turn" or the tool loop limit is hit.
func (a *Agent) Run(ctx context.Context, userInput string) (string, error) {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return "", fmt.Errorf("agent: empty input")
	}
	if len(a.Session.Messages) >= a.MaxTurns*2 {
		return "", fmt.Errorf("agent: session has reached the maximum of %d turns", a.MaxTurns)
	}

	systemPrompt, err := a.buildSystemPrompt()
	if err != nil {
		return "", fmt.Errorf("agent: build system prompt: %w", err)
	}

	// Append the user turn.
	a.Session.Messages = append(a.Session.Messages, api.Message{
		Role:    "user",
		Content: userInput,
	})

	var finalText string

	for iteration := 0; iteration < defaultMaxToolLoop; iteration++ {
		req := api.MessagesRequest{
			Model:     a.Config.Model,
			MaxTokens: a.Config.MaxTokens,
			System:    systemPrompt,
			Messages:  a.windowedHistory(),
			Stream:    true,
			Tools:     a.Tools.Definitions(),
		}

		var textBuf strings.Builder
		onDelta := func(text string) {
			textBuf.WriteString(text)
			if a.onOutput != nil {
				a.onOutput(text)
			}
		}

		resp, err := a.Client.StreamMessage(ctx, req, onDelta)
		if err != nil {
			// Roll back the user message on first iteration only.
			if iteration == 0 {
				a.Session.Messages = a.Session.Messages[:len(a.Session.Messages)-1]
			}
			return "", fmt.Errorf("agent: api call: %w", err)
		}

		a.TokensUsed += resp.Usage.InputTokens + resp.Usage.OutputTokens

		// Record what came back as text so far.
		if textBuf.Len() > 0 {
			finalText = textBuf.String()
		}

		// Add the assistant's full response (may include tool_use blocks) to history.
		a.Session.Messages = append(a.Session.Messages, api.Message{
			Role:    "assistant",
			Content: resp.Content,
		})

		if resp.StopReason != "tool_use" {
			break
		}

		// Execute tool calls and collect results.
		var results []api.ContentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}
			if a.onOutput != nil {
				a.onOutput("\x00TOOL\x00" + block.Name)
			}
			output, execErr := a.Tools.Execute(ctx, block.Name, block.Input)
			isError := false
			if execErr != nil {
				output = execErr.Error()
				isError = true
			}
			results = append(results, api.ContentBlock{
				Type:      "tool_result",
				ToolUseID: block.ID,
				Content:   output,
				IsError:   isError,
			})
		}

		// Send tool results back as a user turn.
		a.Session.Messages = append(a.Session.Messages, api.Message{
			Role:    "user",
			Content: results,
		})
	}

	return finalText, nil
}

// Reset clears in-memory conversation history. DB and memory files untouched.
func (a *Agent) Reset() {
	a.Session.Messages = []api.Message{}
}

// windowedHistory returns at most 2*HistoryWindow messages from the tail of
// the full history. Always starts on a user message.
func (a *Agent) windowedHistory() []api.Message {
	msgs := a.Session.Messages
	limit := a.Config.HistoryWindow * 2
	if len(msgs) <= limit {
		return msgs
	}
	drop := len(msgs) - limit
	if drop%2 != 0 {
		drop++
	}
	return msgs[drop:]
}

// buildSystemPrompt builds the system prompt:
//  1. Soul file (persistent identity)
//  2. Current date
//  3. Session memory (MEMORY.md) if non-empty
func (a *Agent) buildSystemPrompt() (string, error) {
	soul, err := a.Memory.ReadSoul()
	if err != nil {
		return "", fmt.Errorf("agent: read soul: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(soul))
	sb.WriteString("\n\n")
	sb.WriteString("Current date: " + time.Now().UTC().Format("2006-01-02") + ".")

	mem, err := a.Memory.Read(a.Session.ID)
	if err != nil {
		return "", fmt.Errorf("agent: read memory: %w", err)
	}
	if strings.TrimSpace(mem) != "" {
		sb.WriteString("\n\n## Session Memory\n\n")
		sb.WriteString(strings.TrimSpace(mem))
	}

	return sb.String(), nil
}
