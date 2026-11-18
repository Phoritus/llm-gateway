// Package cache holds the exact-match cache: the key a request hashes to, the
// two predicates that decide what may be cached, the stored entry and its
// replay as a stream, and the Redis store behind them. It also holds the
// embedding families and Embeddings, which embeds a text through a workspace's
// credentials for the Phase 2 semantic cache, failing over only inside one
// family.
package cache
