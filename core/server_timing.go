//go:build timing

// Times of the steps of a request, sent to the browser in the `Server-Timing` header.
// DevTools shows them in the Timing tab of the request.
// This file is built only with the `timing` build tag (`go build -tags timing`).
// A build without it uses `server_timing_off.go`, whose functions of the same names do nothing.
//
// NOTE: The timing only watches. It must not change the order, buffering or concurrency of what it times.
// A step that can't be timed without such a change isn't timed.
// For example, the gzip of a file answer runs while the body is sent, after the headers are gone, so it isn't timed.
package core

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// When this function instance started, and how many requests it has served through `ServeAPIRequest`.
// They show whether a request came to a new instance (a cold start).
var (
	instanceStartedAt = time.Now()
	instanceRequests  atomic.Int64 // `atomic.Int64`: a counter that many goroutines can add to without a lock
)

// The step times of one request.
// Goroutines of the same request can add to it, so it has a lock.
type serverTimings struct {
	requestedAt time.Time  // When the request started. `total` is measured from it
	instance    string     // Which request of the instance this is, and how long the instance has been up
	mu          sync.Mutex // Lock for `entries`
	entries     []string   // `Server-Timing` items like `github-read;dur=242.1;desc="db/star-items"`, in the order the steps ended
}

// Key of the `serverTimings` in a `ctx`.
// It is a type of its own, so a value that other code puts in the `ctx` can't use the same key
type serverTimingsKey struct{}

// Returns a copy of `ctx` that carries new, empty step times for one request.
// It counts the request for the instance and starts the clock of `total`, so call it once per request.
func withServerTimings(ctx context.Context) context.Context {
	timings := &serverTimings{
		requestedAt: time.Now(),
		instance:    fmt.Sprintf("request %d, up %ds", instanceRequests.Add(1), int(time.Since(instanceStartedAt).Seconds())),
	}
	return context.WithValue(ctx, serverTimingsKey{}, timings)
}

// One step being timed (`beginServerTimingStep`).
// Ending it adds the step to the step times of the request.
type serverTimingStep struct {
	timings *serverTimings // The step times of the request. `nil` if `ctx` carries none, and then ending does nothing
	name    string         // Step name shown in DevTools
	start   time.Time      // When the step started
}

// Starts timing one step named `name`, shown in DevTools.
// Call `end` or `endWithDesc` on the result when the step ends.
// The step isn't timed if `ctx` carries no step times (a `ctx` that `ServeAPIRequest` didn't make).
//
// Args:
//   - `name`: Letters, digits and `-` only, so it is always a valid name in the header
func beginServerTimingStep(ctx context.Context, name string) serverTimingStep {
	timings, _ := ctx.Value(serverTimingsKey{}).(*serverTimings) // Type assertion (like `dynamic_cast`). Without the value, `timings` is `nil`
	return serverTimingStep{timings: timings, name: name, start: time.Now()}
}

// Ends the step, and adds it without a description.
func (s serverTimingStep) end() { s.endWithDesc("") }

// Ends the step, and adds it with `desc` shown next to it in DevTools.
func (s serverTimingStep) endWithDesc(desc string) {
	if s.timings == nil {
		return
	}
	entry := fmt.Sprintf("%s;dur=%.1f", s.name, float64(time.Since(s.start).Microseconds())/1000)
	if desc != "" {
		entry += fmt.Sprintf(";desc=%q", desc)
	}
	s.timings.mu.Lock()
	defer s.timings.mu.Unlock()
	s.timings.entries = append(s.timings.entries, entry)
}

// Wraps the response writer `w`, so the answer carries the step times in `ctx` as a `Server-Timing` header.
// It returns `w` itself if `ctx` carries no step times.
//
// NOTE: The headers go out when the handler writes the status or the first bytes of the body.
// Setting a header after that has no effect, so the wrapper adds it at that moment.
func wrapResponseWriterWithServerTiming(ctx context.Context, w http.ResponseWriter) http.ResponseWriter {
	timings, _ := ctx.Value(serverTimingsKey{}).(*serverTimings)
	if timings == nil {
		return w
	}
	return &serverTimingWriter{ResponseWriter: w, timings: timings}
}

// Response writer that adds the step times as a `Server-Timing` header, just before the headers go out.
// The embedded `http.ResponseWriter` is the wrapped writer.
// Every method that isn't written here goes to it unchanged (Go's struct embedding).
//
// NOTE: The wrapper doesn't have the optional methods of the wrapped writer (`http.Flusher`, `http.Hijacker`, `io.ReaderFrom`).
// Code that checks for them with a type assertion, or `http.MaxBytesReader`, would act differently in a `timing` build.
// The server has none of them now.
// Use `http.ResponseController`, which reaches the wrapped writer through `Unwrap`.
type serverTimingWriter struct {
	http.ResponseWriter
	timings    *serverTimings // The step times of this request
	headerSent bool           // `true` once the headers went out. The header is added only once
}

// Adds the header, then writes the status code with the wrapped writer.
// An informational status like 103 goes out before the final one, so the header is added only from status 200 on.
func (w *serverTimingWriter) WriteHeader(status int) {
	if status >= 200 {
		w.addHeaderOnce()
	}
	w.ResponseWriter.WriteHeader(status)
}

// Adds the header, then writes body bytes with the wrapped writer.
// Writing the body without `WriteHeader` sends the headers with status 200 (Go's `net/http`).
// The wrapped writer does that by itself, so this doesn't call `WriteHeader`.
func (w *serverTimingWriter) Write(b []byte) (int, error) {
	w.addHeaderOnce()
	return w.ResponseWriter.Write(b)
}

// Adds the `Server-Timing` header, the first time only.
func (w *serverTimingWriter) addHeaderOnce() {
	if !w.headerSent {
		w.headerSent = true
		w.Header().Set("Server-Timing", w.timings.format())
	}
}

// Returns the wrapped writer.
// `http.ResponseController` follows it to reach features of the original writer (like flushing).
func (w *serverTimingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Formats the steps that ended so far, then `total` (time since the request started),
// with the Go version and the instance in its description.
func (t *serverTimings) format() string {
	t.mu.Lock()
	entries := append([]string{}, t.entries...) // A copy, so the lock isn't held while formatting
	t.mu.Unlock()
	total := fmt.Sprintf("total;dur=%.1f;desc=%q", float64(time.Since(t.requestedAt).Microseconds())/1000, runtime.Version()+", "+t.instance)
	return strings.Join(append(entries, total), ", ")
}
