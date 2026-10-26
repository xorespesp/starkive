// Unit tests for the error answers, the request deadline and the rate limit mark of `ServeAPIRequest` (`api_response.go`)
package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests the `ctx` that `ServeAPIRequest` gives the handler.
// Its deadline is `requestTimeLimit` from now.
// It isn't cancelled when the request's own `ctx` is (the browser went away), so a write that has started still ends
func TestServeAPIRequestGivesRequestDeadline(t *testing.T) {
	browserGone, leave := context.WithCancel(context.Background())
	leave()
	req := httptest.NewRequest("GET", "http://example.test/api/archive-star-items", nil).WithContext(browserGone)
	var deadline time.Time
	var hasDeadline bool
	var ctxErr error
	ServeAPIRequest(httptest.NewRecorder(), req, func(w http.ResponseWriter, r *http.Request) error {
		deadline, hasDeadline = r.Context().Deadline()
		ctxErr = r.Context().Err()
		return nil
	})
	if timeLeft := time.Until(deadline); !hasDeadline || timeLeft > requestTimeLimit || timeLeft < requestTimeLimit-time.Minute || ctxErr != nil {
		t.Errorf("deadline %v (set %v), error %v", deadline, hasDeadline, ctxErr)
	}
}

// Tests that `ServeAPIRequest` gives each request its own unset `gitHubRateLimitMark`.
// A rate limit answer in one request must not hold back the queries of the next one
func TestServeAPIRequestGivesNewRateLimitMark(t *testing.T) {
	marks := []*gitHubRateLimitMark{}
	for range 2 {
		req := httptest.NewRequest("POST", "http://example.test/api/sync-archive", nil)
		ServeAPIRequest(httptest.NewRecorder(), req, func(w http.ResponseWriter, r *http.Request) error {
			mark := readGitHubRateLimitMark(r.Context())
			marks = append(marks, mark)
			mark.recordIfRateLimited(fmt.Errorf("%w: GitHub 403", ErrGitHubRateLimited))
			return nil
		})
	}
	if len(marks) != 2 || marks[0] == nil || marks[1] == nil || marks[0] == marks[1] {
		t.Errorf("marks %v", marks)
	}
}

// Tests that a panic in the handler is answered with 500, like any other error, and its stack goes to the server log
func TestServeAPIRequestAnswersPanicWith500(t *testing.T) {
	var logged bytes.Buffer
	savedOutput := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(savedOutput) })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://example.test/api/archive-star-items", nil)
	ServeAPIRequest(rec, req, func(w http.ResponseWriter, r *http.Request) error {
		var filesAtCommit DBRepoFilesAtCommit
		filesAtCommit.FileContent(StarListsDBPath) // A path that wasn't read panics
		return nil
	})
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Internal server error") ||
		!strings.Contains(logged.String(), "wasn't read") || !strings.Contains(logged.String(), "goroutine") {
		t.Errorf("answer %d %s, log %q", rec.Code, rec.Body, logged.String())
	}
}

// Tests that `RunWithPanicAsError` returns the error of `f` as it is, and turns a panic into an error
func TestRunWithPanicAsError(t *testing.T) {
	want := errors.New("plain error")
	if err := RunWithPanicAsError(func() error { return want }); err != want {
		t.Errorf("plain error: %v", err)
	}
	if err := RunWithPanicAsError(func() error { panic("boom") }); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("panic: %v", err)
	}
}

// Tests that `ServeAPIRequest` writes the cause of a 5xx answer to the server log when the answer leaves it out.
// The browser gets only the message, and the server log gets the cause (here, why GitHub couldn't be reached)
func TestServeAPIRequestLogsTheCauseOf502(t *testing.T) {
	var logged bytes.Buffer
	savedOutput := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(savedOutput) })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://example.test/api/archive-star-items", nil)
	ServeAPIRequest(rec, req, func(w http.ResponseWriter, r *http.Request) error {
		return newGitHubUnreachableError(errors.New("dial tcp: connection refused"))
	})
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "dial tcp") || !strings.Contains(logged.String(), "dial tcp: connection refused") {
		t.Errorf("answer %d %s, log %q", rec.Code, rec.Body, logged.String())
	}
}
