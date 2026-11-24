// Package handoff makes a conversation fit a smaller context window without
// breaking it, and knows the windows to fit to before any upstream has said.
//
// It holds no state and calls nothing: Fit is a pure function over a request,
// Budget turns a window into the gateway's estimated tokens, and Window reads
// a catalogue built offline from published model data. The router decides when
// to use them; windows learned from upstream errors live in its shared store.
package handoff
