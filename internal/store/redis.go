package store

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisOptions parses a Redis URL into the options every client here uses.
// ContextTimeoutEnabled makes a context's deadline reach the socket: without
// it go-redis ignores the deadline and waits out its own 3s read timeout, so
// a hung Redis would hold every bounded call (rate limits, the caches, the
// budget check) for seconds.
func RedisOptions(redisURL string) (*redis.Options, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}
	opts.ContextTimeoutEnabled = true
	return opts, nil
}

// OpenRedis connects to Redis and verifies it answers before returning.
func OpenRedis(ctx context.Context, redisURL string) (*redis.Client, error) {
	opts, err := RedisOptions(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping Redis: %w", err)
	}
	return client, nil
}
