// Package batch is a bounded, non-blocking queue that a background loop
// drains into a store in batches. Usage events and prompt logs both use it:
// the data plane records into it on the request path, so Record never waits,
// a full queue drops its oldest item, and every loss is counted by reason.
package batch
