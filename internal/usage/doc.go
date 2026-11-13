// Package usage is the write side of usage history: a non-blocking queue the
// data plane records into, a writer that batches it into Postgres, and the
// retention job that prunes raw events. Logging never delays a response.
//
// The read side is not here. The dashboard reads the daily rollup and a short
// recent list straight from the store, so the two can change independently.
package usage
