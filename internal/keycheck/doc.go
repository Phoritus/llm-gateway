// Package keycheck asks each credential's provider, once a day, whether it
// still accepts the key, and records the answer on the credential. It calls
// the adapter's CheckKey, never the router, so skip and breaker state stay
// driven by real traffic. Check and Result also serve the manual key test.
package keycheck
