// Unit tests for the error answers, the request deadline and the rate limit mark of `ServeAPIRequest` (`api_response.go`)
package core

import (
	"bytes"
	"context"
	"encoding/base64"
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

// Tests that our `ETag` gives back the source `ETag` it was made from, and only to a request it fits.
//   - It is found among several values, and also after a proxy made it weak (`W/`)
//   - A source `ETag` whose base64 has a `-` comes back whole
//   - A value of another build, another encoding, or another shape gives nothing, so the file is read again
//   - Without a build commit or a source `ETag`, there is no `ETag` at all
func TestAPIETagGivesBackTheSourceETag(t *testing.T) {
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "0123456789abcdef0123456789abcdef01234567")
	newRequest := func(gzip bool, ifNoneMatch string) *http.Request {
		r := httptest.NewRequest("GET", "http://example.test/api/archive-star-lists", nil)
		if gzip {
			r.Header.Set("Accept-Encoding", "gzip")
		}
		r.Header.Set("If-None-Match", ifNoneMatch)
		return r
	}
	const source = `W/">>>"` // Its base64 (`Vy8iPj4-Ig`) has a `-`
	if !strings.Contains(base64.RawURLEncoding.EncodeToString([]byte(source)), "-") {
		t.Fatal("the sample source ETag must have a - in its base64")
	}
	plain := buildAPIETag(newRequest(false, ""), source)
	gzipped := buildAPIETag(newRequest(true, ""), source)
	if plain == "" || gzipped == "" || plain == gzipped {
		t.Fatalf("ETags: plain %q, gzip %q", plain, gzipped)
	}

	for name, c := range map[string]struct {
		gzip        bool
		ifNoneMatch string
		want        string
	}{
		"same":             {false, plain, source},
		"weak":             {false, "W/" + plain, source},
		"among others":     {true, `"other", ` + gzipped + `, "last"`, source},
		"other encoding":   {false, gzipped, ""},
		"other shape":      {false, `"sk0` + strings.TrimPrefix(plain, `"sk1`), ""},
		"not base64":       {false, `"sk1-0123456789abcdef0123456789abcdef01234567-id-!!"`, ""},
		"not ours":         {false, `"abc"`, ""},
		"no If-None-Match": {false, "", ""},
	} {
		if got := parseSourceETagFromAPIETag(newRequest(c.gzip, c.ifNoneMatch)); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}

	t.Setenv("VERCEL_GIT_COMMIT_SHA", "fedcba9876543210fedcba9876543210fedcba98") // A new build
	if got := parseSourceETagFromAPIETag(newRequest(false, plain)); got != "" {
		t.Errorf("other build: %q", got)
	}
	if got := buildAPIETag(newRequest(false, ""), ""); got != "" {
		t.Errorf("no source ETag: %q", got)
	}
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "")
	if got := buildAPIETag(newRequest(false, ""), source); got != "" {
		t.Errorf("no build commit: %q", got)
	}
}
