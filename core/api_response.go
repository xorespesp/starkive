// Making the answers of our API.
// It covers error answers (`HTTPError`, `Errorf`), running a handler under the request deadline (`requestTimeLimit`, `ServeAPIRequest`),
// and JSON answers, including a DB repository file sent as it is.
// Reading and checking the requests is in `api_request.go`.
package core

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error answer that our API sends to the browser.
// If a handler returns this value as an error, `ServeAPIRequest` answers with `Status` and `{"error": Message}`
type HTTPError struct {
	Status  int         // Response status code
	Message string      // Message for the browser (English)
	Header  http.Header // Response headers to send with it. `nil` if there are none
}

// Implements Go's `error` interface.
// With this method, `*HTTPError` can be returned as an `error`
func (e *HTTPError) Error() string { return e.Message }

// Makes an `HTTPError` from status code `status` and a message in `fmt.Sprintf` format
func Errorf(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Adds one header (`key: value`) to the error answer.
// It returns itself, so calls can be chained like `Errorf(...).addHeader(...)`
func (e *HTTPError) addHeader(key string, value string) *HTTPError {
	if e.Header == nil {
		e.Header = http.Header{}
	}
	e.Header.Add(key, value)
	return e
}

// Time from the start of a request to the deadline of its `ctx` (`ServeAPIRequest`).
// A Vercel function is stopped at 300 seconds (measured, with or without `maxDuration` in `vercel.json`).
// The 10 seconds left let the server still answer and log why it stopped
const requestTimeLimit = 290 * time.Second

// Runs a handler and turns the returned error into a JSON error answer.
// The entry function of each API wraps its per-method handlers with this function.
// The handler gets the request with a `ctx` whose deadline is `requestTimeLimit` from now (`r.Context()`).
// The `ctx` also carries a new `gitHubRateLimitMark`, so GraphQL queries stop for the rest of the request after a rate limit answer,
// and a new `dbRepoFileCache`, so a DB repository file read again in the request downloads nothing if it didn't change.
//   - `HTTPError`: answers with its status code and message.
//     For a 5xx whose error wraps a cause, the whole error goes to the server log
//   - `GitHubAPIError` (GitHub answered with a failure): always answers 502
//   - Any other error: answers 500 "Internal server error", and writes the details only to the server log.
//     A panic in the handler is answered the same way (`RunWithPanicAsError`).
//     If the handler had already started its answer, the status can't change any more, and the log shows the cause
//
// In a build with the `timing` build tag, the answer also carries the times of the steps (`Server-Timing`, `server_timing.go`).
//
// Args:
//   - `handle`: Per-method handler. Returns `nil` when it has written the whole answer, or an error on failure
//
// NOTE: The `ctx` doesn't stop when the browser goes away (`context.WithoutCancel`).
// Otherwise closing the tab right after saving would stop a commit halfway, and the save would be lost.
// It stops only at its deadline, before Vercel stops the function.
// GitHub's status codes aren't used as our status codes.
// The page treats 401 as "session expired" and closes the session.
// If a 401 from GitHub rejecting the server token passed through, a token problem would look like a sign-out.
// The same goes for a 409 from GitHub being read as our save conflict (409).
func ServeAPIRequest(w http.ResponseWriter, r *http.Request, handle func(http.ResponseWriter, *http.Request) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestTimeLimit)
	defer cancel() // `cancel` frees the timer of the deadline when the request ends
	ctx = withGitHubRateLimitMark(ctx)
	ctx = withServerTimings(ctx)
	ctx = withDBRepoFileCache(ctx)
	// The handler and the error answers below write through `wrappedWriter`.
	// In a `timing` build, it adds the step times as a header when the headers go out. Otherwise it is `w` itself
	wrappedWriter := wrapResponseWriterWithServerTiming(ctx, w)
	err := RunWithPanicAsError(func() error { return handle(wrappedWriter, r.WithContext(ctx)) })
	if err == nil {
		return
	}

	var httpErr *HTTPError
	if errors.As(err, &httpErr) { // If `err` is a `*HTTPError` or wraps one with `%w`, it is taken out into `httpErr` (like `dynamic_cast`)
		if httpErr.Status >= 500 && err != error(httpErr) { // The error wraps a cause (e.g. why GitHub couldn't be reached) that the answer leaves out
			log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
		}
		for key, values := range httpErr.Header {
			for _, v := range values {
				wrappedWriter.Header().Add(key, v)
			}
		}
		WriteAPIJSONResponse(wrappedWriter, httpErr.Status, map[string]string{"error": httpErr.Message})
		return
	}
	var githubErr *GitHubAPIError
	if errors.As(err, &githubErr) {
		WriteAPIJSONResponse(wrappedWriter, http.StatusBadGateway, map[string]string{"error": githubErr.Error()})
		return
	}

	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	WriteAPIJSONResponse(wrappedWriter, http.StatusInternalServerError, map[string]string{"error": "Internal server error"})
}

// Answers 405 to an HTTP method that isn't supported
func WriteAPIMethodNotAllowed(w http.ResponseWriter) {
	WriteAPIJSONResponse(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
}

// Sets the headers of a JSON answer.
// Every JSON answer goes through this function.
//
// NOTE: Answers contain personal data (notes, star lists).
// `Cache-Control: no-store` keeps them out of the browser cache and caches on the way.
func setAPIJSONResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
}

// Writes value `body` (a struct, a map, and so on) as a JSON answer with status code `status`
func WriteAPIJSONResponse(w http.ResponseWriter, status int, body any) {
	setAPIJSONResponseHeaders(w)
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // By default `<`, `>` and `&` become Unicode escapes, in case the output goes into HTML. Answers don't go into HTML, so it is off
	_ = enc.Encode(body)     // If writing fails, the headers are already sent and nothing can be done, so the error is dropped
}

// Answers with a data file of the DB repository as it is.
// If the browser accepts it, the answer is compressed with gzip.
//
// Args:
//   - `path`: File path in the DB repository (`StarListsDBPath` and so on). The archive items have their own answer, `WriteAPIAllStarItemsResponse`
//
// Returns:
//   - The error if the file couldn't be read. If the file doesn't exist yet, it answers with an empty list `[]`
//
// NOTE: A data file is already the JSON array the browser needs, so it isn't decoded and encoded again.
func WriteAPIDBFileResponse(w http.ResponseWriter, r *http.Request, repo *DBRepo, path string) error {
	ctx := r.Context() // The deadline that `ServeAPIRequest` set for this request
	timingStep := beginServerTimingStep(ctx, "github-read")
	content, err := ReadDBRepoFile(ctx, repo, path)
	timingStep.endWithDesc(path)
	if err != nil {
		return err
	}
	if content == nil {
		content = []byte("[]")
	}
	return writeAPIBodyGzipIfAccepted(w, r, content)
}

// Answers with all archive items as one JSON array, one item per line.
// It reads every shard at the latest commit (`shardedStarItemsDataFile.readLatestFromDBRepo`), so the items are from one commit and are checked.
// If the browser accepts it, the answer is compressed with gzip.
func WriteAPIAllStarItemsResponse(w http.ResponseWriter, r *http.Request, repo *DBRepo) error {
	ctx := r.Context() // The deadline that `ServeAPIRequest` set for this request
	timingStep := beginServerTimingStep(ctx, "github-read")
	items, err := starItemsDataFile.readLatestFromDBRepo(ctx, repo)
	timingStep.endWithDesc(StarItemsDBDir)
	if err != nil {
		return err
	}
	content, err := encodeJSONArrayByLine(items)
	if err != nil {
		return err
	}
	return writeAPIBodyGzipIfAccepted(w, r, content)
}

// Writes the JSON array `content` as the answer, compressed with gzip if the browser accepts it.
//
// NOTE: The whole archive goes in one answer, so Vercel's response limit (4.5MB, docs) caps the archive size.
// Compression makes it about 4 times smaller (measured).
func writeAPIBodyGzipIfAccepted(w http.ResponseWriter, r *http.Request, content []byte) error {
	setAPIJSONResponseHeaders(w)
	w.Header().Add("Vary", "Accept-Encoding") // Tells that the body differs by request header even at the same address
	if !isGzipAccepted(r) {
		_, err := w.Write(content)
		return err
	}
	w.Header().Set("Content-Encoding", "gzip")
	zw := gzip.NewWriter(w) // Writer that compresses what is written and passes it on to `w`. `Close` writes the remaining data and the end mark
	if _, err := zw.Write(content); err != nil {
		return err
	}
	return zw.Close()
}

// Reports whether the request's `Accept-Encoding` says it accepts gzip.
// The value is a comma-separated list like `gzip, deflate, br`.
// Each item can have a preference like `;q=0.5`.
// `q=0` means "not accepted" (docs: RFC 9110).
func isGzipAccepted(r *http.Request) bool {
	for _, item := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(item, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		q, hasQ := strings.CutPrefix(strings.TrimSpace(params), "q=")
		if !hasQ {
			return true // No preference means 1
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
		return err == nil && value > 0
	}
	return false
}
