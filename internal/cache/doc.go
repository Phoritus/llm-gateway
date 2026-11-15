// Package cache holds the exact-match cache: the key a request hashes to, the
// two predicates that decide what may be cached, the stored entry and its
// replay as a stream, and the Redis store behind them. The Phase 2 semantic
// cache and its embedding adapters will live here too.
package cache
