package api

import "encoding/json"

// MessagesRequest is the request body for the Anthropic Messages API.
type MessagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream,omitempty"`
	Tools     []Tool    `json:"tools,omitempty"`
}

// Message represents a single conversation turn.
// Content can be a plain string or a slice of ContentBlock (for tool use, images, etc.).
type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

// ContentBlock is a typed content element within a message.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// MessagesResponse is the response body from the Anthropic Messages API.
type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Usage reports token consumption for a request.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// StreamEvent is a single server-sent event from the streaming endpoint.
type StreamEvent struct {
	Type    string            `json:"type"`
	Delta   *Delta            `json:"delta,omitempty"`
	Message *MessagesResponse `json:"message,omitempty"`
	Index   int               `json:"index"`
}

// Delta carries incremental content within a streaming event.
type Delta struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
}

// Tool describes a callable tool exposed to the model.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicError is the JSON envelope Anthropic returns for API errors.
type anthropicError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}
