// Package store holds the Postgres queries. Every tenant query is scoped by
// workspace id, which handlers take from the session and never from the body.
package store
