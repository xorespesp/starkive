//go:build !timing

// The step timing of `server_timing.go` for a build without the `timing` build tag.
// The functions have the same names and do nothing, so the server's normal build sends no `Server-Timing` header.
// The compiler inlines these empty functions, so the places that call them cost nothing.
package core

import (
	"context"
	"net/http"
)

// Returns `ctx` unchanged.
func withServerTimings(ctx context.Context) context.Context { return ctx }

// A step that isn't timed. It holds nothing
type serverTimingStep struct{}

// Returns a step whose ending does nothing.
func beginServerTimingStep(ctx context.Context, name string) serverTimingStep {
	return serverTimingStep{}
}

// Does nothing.
func (s serverTimingStep) end() {}

// Does nothing.
func (s serverTimingStep) endWithDesc(desc string) {}

// Returns `w` unchanged.
func wrapResponseWriterWithServerTiming(ctx context.Context, w http.ResponseWriter) http.ResponseWriter {
	return w
}
