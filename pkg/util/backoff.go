package util

import (
	"context"
	"github.com/cenkalti/backoff/v5"
	"time"
)

// RetryConfig provides standard retry configurations
type RetryConfig struct {
	InitialInterval     time.Duration
	MaxInterval         time.Duration
	MaxElapsedTime      time.Duration
	RandomizationFactor float64
}

var (
	// EtcdRetry is configured for etcd operations
	EtcdRetry = RetryConfig{
		InitialInterval:     2 * time.Second,
		MaxInterval:         30 * time.Second,
		RandomizationFactor: 0.2,
	}

	// ConfigRetry is configured for Prometheus configuration operations
	ConfigRetry = RetryConfig{
		InitialInterval:     1 * time.Second,
		MaxInterval:         5 * time.Second,
		RandomizationFactor: 0.1,
	}
)

// RetryOperation executes an operation with retry logic
func RetryOperation[T any](operation func() (T, error), config RetryConfig, maxRetries uint) (T, error) {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = config.InitialInterval
	b.MaxInterval = config.MaxInterval
	b.RandomizationFactor = config.RandomizationFactor

	var result T
	ctx := context.Background()

	result, err := backoff.Retry(
		ctx,
		operation,
		backoff.WithMaxTries(maxRetries),
		backoff.WithBackOff(b),
	)

	return result, err
}
