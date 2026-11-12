// Package crypto seals and opens provider credentials with AES-256-GCM.
// A sealed value records which master key sealed it, so the key can be
// rotated by re-sealing rows whose key id is not the current one.
package crypto
