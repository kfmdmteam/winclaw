package api

import "encoding/json"

// Message is a single conversation turn.
// Content is string for simple user messages, or []ContentBlock for
// assistant messages (which may contain text + tool_use blocks) and
// user messages that carry tool_result blocks.
type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string | []ContentBlock
}

// ContentBlock is one typed block inside a message.
type ContentBlock struct {
	Type string `json:"type"`

	// type:"text"
	Text string `json:"text,omitempty"`

	// type:"tool_use"  (assistant → client)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type:"tool_result"  (client → assistant)
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// MessagesRequest is POST /v1/messages body.
type MessagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream"`
	Tools     []Tool    `json:"tools,omitempty"`
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
}

// anthropicError is the error envelope.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
