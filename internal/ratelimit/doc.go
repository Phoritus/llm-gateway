// Package ratelimit implements the token bucket: in Redis as Lua scripts, so
// every instance agrees, and in process for when Redis is unreachable.
package ratelimit
