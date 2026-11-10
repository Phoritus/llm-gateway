// Package config loads the service configuration from environment variables
// only. It refuses to produce a Config when a required secret is missing, so
// the process fails at startup rather than at the first request that needs it.
package config
