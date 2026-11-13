package router

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Redis is the Store every instance shares. Its keys follow the spec's Redis
// table (skip:..., cb:...) and every one of them expires.
type Redis struct {
	client *redis.Client
	prefix string
}

func NewRedis(client *redis.Client) *Redis {
	return &Redis{client: client}
}

func (s *Redis) Get(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	raw, err := s.client.MGet(ctx, s.prefixed(keys)...).Result()
	if err != nil {
		return nil, fmt.Errorf("router: read skip state: %w", err)
	}
	values := make([]string, len(keys))
	for i, v := range raw {
		values[i], _ = v.(string)
	}
	return values, nil
}

func (s *Redis) Incr(ctx context.Context, counters []Counter) ([]int64, error) {
	if len(counters) == 0 {
		return nil, nil
	}
	pipe := s.client.Pipeline()
	incrs := make([]*redis.IntCmd, len(counters))
	for i, c := range counters {
		incrs[i] = pipe.Incr(ctx, s.prefix+c.Key)
		pipe.ExpireNX(ctx, s.prefix+c.Key, c.Window)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("router: count failures: %w", err)
	}
	counts := make([]int64, len(counters))
	for i, cmd := range incrs {
		counts[i] = cmd.Val()
	}
	return counts, nil
}

func (s *Redis) Set(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := s.client.Pipeline()
	for _, e := range entries {
		pipe.Set(ctx, s.prefix+e.Key, e.Value, e.TTL)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("router: store skips: %w", err)
	}
	return nil
}

func (s *Redis) Del(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := s.client.Del(ctx, s.prefixed(keys)...).Err(); err != nil {
		return fmt.Errorf("router: clear counters: %w", err)
	}
	return nil
}

func (s *Redis) prefixed(keys []string) []string {
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = s.prefix + key
	}
	return out
}
