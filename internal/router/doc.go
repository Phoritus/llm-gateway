// Package router tries a route's targets in order until one answers. It owns
// the skip rules and the circuit breaker, whose state every instance shares
// through Redis, and will own free-first ordering.
package router
