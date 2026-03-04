package api

import (
	"context"
	"sync"
	"time"
)

const (
	defaultRequestsPerMinute = 50
	pollInterval             = 10 * time.Millisecond
)

// RateLimiter is a token bucket rate limiter suitable for API request throttling.
type RateLimiter struct {
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second
	mu         sync.Mutex
	lastRefill time.Time
}

// NewRateLimiter creates a RateLimiter that allows up to requestsPerMinute
// requests per minute. Pass 0 to use the default (50 req/min).
func NewRateLimiter(requestsPerMinute int) *RateLimiter {
	if requestsPerMinute <= 0 {
		requestsPerMinute = defaultRequestsPerMinute
	}
	max := float64(requestsPerMinute)
	return &RateLimiter{
		tokens:     max,
		maxTokens:  max,
		refillRate: max / 60.0, // tokens per second
		lastRefill: time.Now(),
	}
}

// Wait blocks until a token is available or the context is cancelled.
// It returns ctx.Err() if the context is done before a token can be obtained.
func (r *RateLimiter) Wait(ctx context.Context) error {
	for {
		// Check context before attempting to acquire a token.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if r.tryConsume() {
			return nil
		}

		// Sleep briefly before retrying to avoid a busy-wait loop.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// tryConsume refills the bucket based on elapsed time and consumes one token
// if available. Returns true on success.
func (r *RateLimiter) tryConsume() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(r.lastRefill).Seconds()
	r.lastRefill = now

	// Add tokens earned since the last call.
	r.tokens += elapsed * r.refillRate
	if r.tokens > r.maxTokens {
		r.tokens = r.maxTokens
	}

	if r.tokens < 1.0 {
		return false
	}

	r.tokens--
	return true
}
