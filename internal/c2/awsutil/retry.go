// Package awsutil provides AWS utilities including retry logic with exponential backoff.
package awsutil

import (
	"context"
	"log/slog"
	"math/rand"
	"time"
)

// DefaultMaxRetries is the default number of retry attempts.
const DefaultMaxRetries = 3

// WithRetry executes an operation with exponential backoff and jitter.
// It retries the operation up to maxRetries times on failure.
// The backoff duration is 2^attempt seconds plus random jitter up to 1 second.
func WithRetry[T any](ctx context.Context, operation string, maxRetries int, fn func() (T, error)) (T, error) {
	var result T
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, lastErr = fn()
		if lastErr == nil {
			return result, nil
		}

		if attempt < maxRetries {
			// Exponential backoff with jitter
			backoff := time.Duration(1<<attempt) * time.Second
			jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
			delay := backoff + jitter

			slog.Warn("AWS API call failed, retrying",
				"operation", operation,
				"attempt", attempt+1,
				"max_retries", maxRetries,
				"delay", delay,
				"error", lastErr)

			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	return result, lastErr
}
