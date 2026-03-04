package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/memory"
)

const defaultMaxTurns = 20

// Agent drives a single session's conversation with the Anthropic API.
// It maintains the in-memory message history for the active session and
// streams assistant tokens to the terminal via the onOutput callback.
type Agent struct {
	Session    *Session
	Client     *api.Client
	Memory     *memory.MemoryManager
	Config     *config.Config
	MaxTurns   int
	TokensUsed int // running total of tokens charged this session
	onOutput   func(text string)
}

// NewAgent constructs an Agent for the given session.
// onOutput is called with each streamed text chunk as it arrives; it may be nil
// if the caller does not need streaming output.
func NewAgent(
	sess *Session,
	client *api.Client,
	mem *memory.MemoryManager,
	cfg *config.Config,
	onOutput func(string),
) *Agent {
	return &Agent{
		Session:  sess,
		Client:   client,
		Memory:   mem,
		Config:   cfg,
		MaxTurns: defaultMaxTurns,
		onOutput: onOutput,
	}
}

// Run processes a single user input, appending it to the session history,
// calling the API, and returning the full assistant response text.
//
// Token efficiency measures applied on every call:
//   - Input is trimmed of leading/trailing whitespace.
//   - Only the most recent HistoryWindow turns are sent to the API; older
//     messages are retained in Session.Messages for auditing but excluded
//     from the wire payload.
//   - The system prompt is compact: date only (not session ID).
func (a *Agent) Run(ctx context.Context, userInput string) (string, error) {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return "", fmt.Errorf("agent: empty input")
	}

	if len(a.Session.Messages) >= a.MaxTurns*2 {
		return "", fmt.Errorf("agent: session %q has reached the maximum of %d turns",
			a.Session.ID, a.MaxTurns)
	}

	systemPrompt, err := a.buildSystemPrompt()
	if err != nil {
		return "", fmt.Errorf("agent: build system prompt: %w", err)
	}

	a.Session.Messages = append(a.Session.Messages, api.Message{
		Role:    "user",
		Content: userInput,
	})

	req := api.MessagesRequest{
		Model:     a.Config.Model,
		MaxTokens: a.Config.MaxTokens,
		System:    systemPrompt,
		Messages:  a.windowedHistory(),
		Stream:    true,
	}

	var buf strings.Builder
	onDelta := func(text string) {
		buf.WriteString(text)
		if a.onOutput != nil {
			a.onOutput(text)
		}
	}

	resp, err := a.Client.StreamMessage(ctx, req, onDelta)
	if err != nil {
		a.Session.Messages = a.Session.Messages[:len(a.Session.Messages)-1]
		return "", fmt.Errorf("agent: stream message: %w", err)
	}

	fullText := buf.String()
	if fullText == "" && len(resp.Content) > 0 {
		for _, block := range resp.Content {
			if block.Type == "text" {
				fullText += block.Text
			}
		}
	}

	a.Session.Messages = append(a.Session.Messages, api.Message{
		Role:    "assistant",
		Content: fullText,
	})

	a.TokensUsed += resp.Usage.InputTokens + resp.Usage.OutputTokens

	return fullText, nil
}

// Reset clears the in-memory conversation history for the current session.
// The database audit log, token counter, and memory file on disk are untouched.
func (a *Agent) Reset() {
	a.Session.Messages = []api.Message{}
}

// windowedHistory returns at most 2*HistoryWindow messages (the most recent
// HistoryWindow turns) from the full session history. This is the payload sent
// to the API; the full slice is kept for audit purposes.
func (a *Agent) windowedHistory() []api.Message {
	msgs := a.Session.Messages
	limit := a.Config.HistoryWindow * 2 // pairs → individual messages
	if len(msgs) <= limit {
		return msgs
	}
	// Always start on a user message so the conversation is well-formed.
	// Drop from the front in pairs.
	drop := len(msgs) - limit
	if drop%2 != 0 {
		drop++
	}
	return msgs[drop:]
}

// buildSystemPrompt constructs the system prompt sent to the model.
//
// Token budget: the prompt is intentionally short. The session ID is omitted
// (it is meaningful to the host but wastes tokens for the model). Only the
// date is included so the model can give time-aware answers. Memory content,
// when present, is appended verbatim — keep MEMORY.md concise to save tokens.
func (a *Agent) buildSystemPrompt() (string, error) {
	var sb strings.Builder

	sb.WriteString("You are WinClaw, a secure Windows terminal AI assistant. ")
	sb.WriteString(time.Now().UTC().Format("2006-01-02") + ".")

	memContent, err := a.Memory.Read(a.Session.ID)
	if err != nil {
		return "", fmt.Errorf("agent: read memory: %w", err)
	}
	trimmed := strings.TrimSpace(memContent)
	if trimmed != "" {
		sb.WriteString("\n\n")
		sb.WriteString(trimmed)
	}

	return sb.String(), nil
}
