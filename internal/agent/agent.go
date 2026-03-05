package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"winclaw/internal/api"
	"winclaw/internal/config"
	"winclaw/internal/memory"
	"winclaw/internal/tools"
)

const (
	defaultMaxTurns       = 20
	defaultMaxToolLoop    = 10
	thinkingBetaHeader    = "interleaved-thinking-2025-05-14"
	defaultThinkingBudget = 10000
	consolidateThreshold  = 8192 // bytes; auto-consolidate session memory above this
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

	// UseThinking enables extended thinking on the next API request.
	UseThinking    bool
	ThinkingBudget int

	// pendingAttachment is an image block to include with the next Run call.
	pendingAttachment *api.ContentBlock
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
		Session:        sess,
		Client:         client,
		Memory:         mem,
		Config:         cfg,
		Tools:          tl,
		MaxTurns:       defaultMaxTurns,
		onOutput:       onOutput,
		ThinkingBudget: defaultThinkingBudget,
	}
}

// SetAttachment stores an image block to be included with the next Run call.
// The attachment is consumed (cleared) once Run is called.
func (a *Agent) SetAttachment(block *api.ContentBlock) {
	a.pendingAttachment = block
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

	systemPrompt, err := a.buildSystemPrompt(ctx)
	if err != nil {
		return "", fmt.Errorf("agent: build system prompt: %w", err)
	}

	// Build the user message, optionally with a pending image attachment.
	var userMsg api.Message
	if a.pendingAttachment != nil {
		userMsg = api.Message{
			Role: "user",
			Content: []api.ContentBlock{
				*a.pendingAttachment,
				{Type: "text", Text: userInput},
			},
		}
		a.pendingAttachment = nil
	} else {
		userMsg = api.Message{Role: "user", Content: userInput}
	}
	a.Session.Messages = append(a.Session.Messages, userMsg)

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

		// Extended thinking.
		if a.UseThinking {
			budget := a.ThinkingBudget
			if budget <= 0 {
				budget = defaultThinkingBudget
			}
			req.Thinking = &api.ThinkingConfig{
				Type:         "enabled",
				BudgetTokens: budget,
			}
			req.Beta = []string{thinkingBetaHeader}
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
			// Roll back the user message on the first iteration only.
			if iteration == 0 {
				a.Session.Messages = a.Session.Messages[:len(a.Session.Messages)-1]
			}
			return "", fmt.Errorf("agent: api call: %w", err)
		}

		a.TokensUsed += resp.Usage.InputTokens + resp.Usage.OutputTokens

		if textBuf.Len() > 0 {
			finalText = textBuf.String()
		}

		// Add the assistant's full response to history.
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
			isError := execErr != nil
			if isError {
				output = execErr.Error()
			}

			// Detect screenshot/image results and build a rich content block.
			if !isError && strings.HasPrefix(output, "IMAGE_BASE64:") {
				b64 := strings.TrimPrefix(output, "IMAGE_BASE64:")
				results = append(results, api.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					RichContent: []api.ContentBlock{
						{
							Type: "image",
							Source: &api.ImageSource{
								Type:      "base64",
								MediaType: "image/png",
								Data:      b64,
							},
						},
						{Type: "text", Text: "Screenshot captured."},
					},
				})
			} else {
				results = append(results, api.ContentBlock{
					Type:      "tool_result",
					ToolUseID: block.ID,
					Content:   output,
					IsError:   isError,
				})
			}
		}

		// Send tool results back as a user turn.
		a.Session.Messages = append(a.Session.Messages, api.Message{
			Role:    "user",
			Content: results,
		})
	}

	// Auto-consolidate session memory in the background when it grows large.
	go a.maybeConsolidate(context.Background())

	return finalText, nil
}

// Reset clears in-memory conversation history. DB and memory files untouched.
func (a *Agent) Reset() {
	a.Session.Messages = []api.Message{}
}

// windowedHistory returns at most 2*HistoryWindow messages from the tail.
// Always starts on a user message.
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

// buildSystemPrompt assembles the system prompt:
//  1. Soul file (persistent identity)
//  2. Current date
//  3. Auto-context (CWD, git branch)
//  4. Global cross-session memory (if non-empty)
//  5. Per-session memory (if non-empty)
func (a *Agent) buildSystemPrompt(ctx context.Context) (string, error) {
	soul, err := a.Memory.ReadSoul()
	if err != nil {
		return "", fmt.Errorf("agent: read soul: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(strings.TrimSpace(soul))
	sb.WriteString("\n\n")
	sb.WriteString("Current date: " + time.Now().UTC().Format("2006-01-02") + ".")

	// Auto-context: working directory and git branch.
	if autoCtx := gatherContext(ctx); autoCtx != "" {
		sb.WriteString("\n\n## Environment\n\n")
		sb.WriteString(autoCtx)
	}

	// Global cross-session memory.
	global, err := a.Memory.ReadGlobal()
	if err == nil && strings.TrimSpace(global) != "" {
		sb.WriteString("\n\n## Global Memory\n\n")
		sb.WriteString(strings.TrimSpace(global))
	}

	// Per-session memory.
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

// maybeConsolidate summarises the session memory file when it exceeds the
// threshold. Runs as a goroutine; failures are silently ignored.
func (a *Agent) maybeConsolidate(ctx context.Context) {
	if a.Memory.MemorySize(a.Session.ID) < consolidateThreshold {
		return
	}
	content, err := a.Memory.Read(a.Session.ID)
	if err != nil || strings.TrimSpace(content) == "" {
		return
	}

	consolidateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req := api.MessagesRequest{
		Model:     a.Config.Model,
		MaxTokens: 2048,
		Messages: []api.Message{
			{
				Role: "user",
				Content: "Consolidate the following session memory notes into concise, well-organized bullet points. " +
					"Remove redundancy but preserve every unique fact, preference, and decision.\n\n" + content,
			},
		},
	}
	resp, err := a.Client.SendMessage(consolidateCtx, req)
	if err != nil || len(resp.Content) == 0 {
		return
	}
	_ = a.Memory.Write(a.Session.ID, "# Memory (consolidated)\n\n"+resp.Content[0].Text)
}

// gatherContext returns a short string describing the current environment
// (working directory, git branch). Returns empty string if nothing interesting.
func gatherContext(ctx context.Context) string {
	var sb strings.Builder
	if cwd, err := os.Getwd(); err == nil {
		sb.WriteString("Working directory: " + cwd + "\n")
	}
	gitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if out, err := exec.CommandContext(gitCtx, "git", "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch := strings.TrimSpace(string(out))
		if branch != "" && branch != "HEAD" {
			sb.WriteString("Git branch: " + branch + "\n")
		}
	}
	return sb.String()
}
