package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultBaseURL        = "https://api.anthropic.com/v1/messages"
	anthropicVersion      = "2023-06-01"
	requestTimeout        = 120 * time.Second
	maxRetries            = 3
)

// Client is an HTTP client for the Anthropic Messages API.
type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	model      string
}

// NewClient creates a new Anthropic API client with TLS 1.2+ and a 120s timeout.
func NewClient(apiKey, model string) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		model:   model,
		httpClient: &http.Client{
			Timeout:   requestTimeout,
			Transport: transport,
		},
	}
}

// SendMessage sends a non-streaming request and returns the full response.
func (c *Client) SendMessage(ctx context.Context, req MessagesRequest) (*MessagesResponse, error) {
	req.Stream = false
	if req.Model == "" {
		req.Model = c.model
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("api: marshal request: %w", err)
	}

	var resp *MessagesResponse
	err = c.withRetry(ctx, func() error {
		httpResp, err := c.doRequest(ctx, body)
		if err != nil {
			return err
		}
		defer httpResp.Body.Close()

		respBody, err := io.ReadAll(httpResp.Body)
		if err != nil {
			return fmt.Errorf("api: read response body: %w", err)
		}

		if httpResp.StatusCode != http.StatusOK {
			return parseAPIError(httpResp.StatusCode, respBody)
		}

		var result MessagesResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return fmt.Errorf("api: unmarshal response: %w", err)
		}
		resp = &result
		return nil
	})

	return resp, err
}

// StreamMessage sends a streaming request, calling onDelta for each text chunk.
// It returns the reconstructed full response once the stream is complete.
func (c *Client) StreamMessage(ctx context.Context, req MessagesRequest, onDelta func(text string)) (*MessagesResponse, error) {
	req.Stream = true
	if req.Model == "" {
		req.Model = c.model
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("api: marshal request: %w", err)
	}

	var finalResp *MessagesResponse
	err = c.withRetry(ctx, func() error {
		httpResp, err := c.doRequest(ctx, body)
		if err != nil {
			return err
		}
		defer httpResp.Body.Close()

		if httpResp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(httpResp.Body)
			return parseAPIError(httpResp.StatusCode, respBody)
		}

		result, err := c.readStream(ctx, httpResp.Body, onDelta)
		if err != nil {
			return err
		}
		finalResp = result
		return nil
	})

	return finalResp, err
}

// doRequest builds and executes the HTTP request to the Anthropic API.
func (c *Client) doRequest(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("api: create request: %w", err)
	}

	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Unwrap context cancellation so callers can detect it directly.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("api: http do: %w", err)
	}
	return resp, nil
}

// readStream consumes an SSE stream, firing onDelta for each text delta.
// It builds and returns a synthetic MessagesResponse from the stream events.
func (c *Client) readStream(ctx context.Context, r io.Reader, onDelta func(text string)) (*MessagesResponse, error) {
	var (
		result      MessagesResponse
		fullText    strings.Builder
		inputTokens int
		outputTokens int
	)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		// Respect context cancellation between lines.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var event StreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			// Skip malformed events rather than aborting.
			continue
		}

		switch event.Type {
		case "message_start":
			if event.Message != nil {
				result.ID = event.Message.ID
				result.Model = event.Message.Model
				result.Role = event.Message.Role
				result.Type = event.Message.Type
				inputTokens = event.Message.Usage.InputTokens
			}

		case "content_block_delta":
			if event.Delta != nil && event.Delta.Type == "text_delta" {
				text := event.Delta.Text
				fullText.WriteString(text)
				if onDelta != nil {
					onDelta(text)
				}
			}

		case "message_delta":
			if event.Delta != nil && event.Delta.StopReason != "" {
				result.StopReason = event.Delta.StopReason
			}

		case "message_stop":
			// Stream is complete; usage totals arrive in message_delta.

		case "ping":
			// Keepalive, ignore.
		}

		// Accumulate output token count from message_delta usage if present.
		// The Anthropic streaming protocol sends usage in a separate field on
		// message_delta events; we capture it via the raw event's message field.
		if event.Message != nil && event.Message.Usage.OutputTokens > 0 {
			outputTokens = event.Message.Usage.OutputTokens
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("api: read stream: %w", err)
	}

	result.Content = []ContentBlock{{Type: "text", Text: fullText.String()}}
	result.Usage = Usage{InputTokens: inputTokens, OutputTokens: outputTokens}
	return &result, nil
}

// withRetry runs fn up to maxRetries times, backing off on retryable errors.
func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	backoff := time.Second
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				backoff *= 2
			}
		}

		err := fn()
		if err == nil {
			return nil
		}

		lastErr = err

		// Only retry on specific status codes.
		apiErr, ok := err.(*APIError)
		if !ok {
			// Non-API errors (network failures, context cancellation) — retry once,
			// but stop immediately on context cancellation.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}

		if !isRetryable(apiErr.StatusCode) {
			return err
		}
	}

	return lastErr
}

// isRetryable returns true for HTTP status codes that warrant a retry.
func isRetryable(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable:
		return true
	}
	return false
}

// APIError wraps an Anthropic API error with its HTTP status code.
type APIError struct {
	StatusCode  int
	ErrorType   string
	ErrorMsg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api: status %d: %s: %s", e.StatusCode, e.ErrorType, e.ErrorMsg)
}

// parseAPIError deserialises an Anthropic error response body into an APIError.
func parseAPIError(statusCode int, body []byte) *APIError {
	var envelope anthropicError
	if err := json.Unmarshal(body, &envelope); err != nil {
		return &APIError{
			StatusCode: statusCode,
			ErrorType:  "unknown",
			ErrorMsg:   string(body),
		}
	}
	return &APIError{
		StatusCode: statusCode,
		ErrorType:  envelope.Error.Type,
		ErrorMsg:   envelope.Error.Message,
	}
}
