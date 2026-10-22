// Unit tests for the error answers and the request deadline of `ServeAPIRequest` (`api_response.go`)
package core

import (
	"bytes"
	"context"
	"errors"
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
