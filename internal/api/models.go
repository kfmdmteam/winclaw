package api

import "encoding/json"

// Message is a single conversation turn.
// Content is string for simple user messages, or []ContentBlock for
// assistant messages (which may contain text + tool_use blocks) and
// user messages that carry tool_result or image blocks.
type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string | []ContentBlock
}

// ContentBlock is one typed block inside a message.
// When used as a tool_result (sent to the API), Content/RichContent hold the
// result text or rich content (text + images). When received from the API,
// only Type, Text, ID, Name, Input, Source, Thinking, and Signature are used.
type ContentBlock struct {
	Type string `json:"type"`

	// type:"text"
	Text string `json:"text,omitempty"`

	// type:"tool_use"  (assistant → client)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type:"tool_result"  (client → assistant); serialised by MarshalJSON.
	ToolUseID   string         `json:"-"` // handled in MarshalJSON
	Content     string         `json:"-"` // simple text result
	RichContent []ContentBlock `json:"-"` // rich result (text + image blocks)
	IsError     bool           `json:"is_error,omitempty"`

	// type:"image"
	Source *ImageSource `json:"source,omitempty"`

	// type:"thinking"
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// MarshalJSON serialises ContentBlock, handling the polymorphic content field
// for tool_result blocks (string or array of ContentBlock).
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type      string          `json:"type"`
		Text      string          `json:"text,omitempty"`
		ID        string          `json:"id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Input     json.RawMessage `json:"input,omitempty"`
		ToolUseID string          `json:"tool_use_id,omitempty"`
		Content   interface{}     `json:"content,omitempty"`
		IsError   bool            `json:"is_error,omitempty"`
		Source    *ImageSource    `json:"source,omitempty"`
		Thinking  string          `json:"thinking,omitempty"`
		Signature string          `json:"signature,omitempty"`
	}
	w := wire{
		Type:      b.Type,
		Text:      b.Text,
		ID:        b.ID,
		Name:      b.Name,
		Input:     b.Input,
		ToolUseID: b.ToolUseID,
		IsError:   b.IsError,
		Source:    b.Source,
		Thinking:  b.Thinking,
		Signature: b.Signature,
	}
	if len(b.RichContent) > 0 {
		w.Content = b.RichContent
	} else if b.Content != "" {
		w.Content = b.Content
	}
	return json.Marshal(w)
}

// ImageSource describes the source of an image content block.
type ImageSource struct {
	Type      string `json:"type"`            // "base64" or "url"
	MediaType string `json:"media_type"`      // "image/png", "image/jpeg", etc.
	Data      string `json:"data,omitempty"`  // base64-encoded bytes (for type:"base64")
	URL       string `json:"url,omitempty"`   // (for type:"url")
}

// ThinkingConfig enables extended thinking (budget_tokens > 0 enables it).
type ThinkingConfig struct {
	Type         string `json:"type"`          // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// MessagesRequest is the POST /v1/messages request body.
type MessagesRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []Message       `json:"messages"`
	Stream    bool            `json:"stream"`
	Tools     []Tool          `json:"tools,omitempty"`
	Thinking  *ThinkingConfig `json:"thinking,omitempty"`
	Beta      []string        `json:"-"` // extra Anthropic-Beta headers; not serialised
}

// Tool defines a tool the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// MessagesResponse is the response from the API.
type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Usage holds token counts.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// StreamEvent is one SSE event from the streaming API.
type StreamEvent struct {
	Type         string            `json:"type"`
	Index        int               `json:"index"`
	Delta        *Delta            `json:"delta,omitempty"`
	Message      *MessagesResponse `json:"message,omitempty"`
	ContentBlock *ContentBlock     `json:"content_block,omitempty"`
}

// Delta is the incremental update in a streaming content_block_delta event.
type Delta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"` // for thinking_delta events
}

// anthropicError is the error envelope returned by the API on failure.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
