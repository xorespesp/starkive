//go:build timing

package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// Format of the header: the steps, then `total` with the Go version and the instance
var serverTimingPattern = regexp.MustCompile(`^w-head;dur=[0-9.]+, github-read;dur=[0-9.]+;desc=` + regexp.QuoteMeta(fmt.Sprintf("%q", StarItemsDBPath)) +
	`, total;dur=[0-9.]+;desc="go[^"]*, request [0-9]+, up [0-9]+s"$`)

// Runs `handle` through `ServeAPIRequest` after two steps, and returns the `Server-Timing` header as it went out
func serveWithTwoSteps(handle func(w http.ResponseWriter, r *http.Request) error) string {
	rec := httptest.NewRecorder()
	ServeAPIRequest(rec, httptest.NewRequest("GET", "http://example.test/api/archive-star-items", nil), func(w http.ResponseWriter, r *http.Request) error {
		timingStep := beginServerTimingStep(r.Context(), "w-head")
		timingStep.end()
		timingStep = beginServerTimingStep(r.Context(), "github-read")
		timingStep.endWithDesc(StarItemsDBPath)
		return handle(w, r)
	})
	return rec.Result().Header.Get("Server-Timing") // `Result` gives the headers as they were when they went out
}

// Tests that the answer carries the steps that ended before the headers went out,
// whether the handler writes the status first, writes the body only, or returns an error.
// A step that ends after the headers went out isn't in them.
func TestServerTimingHeader(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter, r *http.Request) error{
		"status first": func(w http.ResponseWriter, r *http.Request) error {
			WriteAPIJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
			beginServerTimingStep(r.Context(), "late").end()
			return nil
		},
		"body only": func(w http.ResponseWriter, r *http.Request) error {
			_, err := w.Write([]byte("[]\n"))
			return err
		},
		"error answer": func(w http.ResponseWriter, r *http.Request) error {
			return errors.New("broken")
		},
	}
	for name, handle := range cases {
		if got := serveWithTwoSteps(handle); !serverTimingPattern.MatchString(got) {
			t.Errorf("%s: %q", name, got)
		}
	}
}

// Tests that the step timing does nothing where `ctx` carries no step times (outside `ServeAPIRequest`)
func TestServerTimingWithoutStepTimes(t *testing.T) {
	beginServerTimingStep(context.Background(), "w-head").end() // Must not panic
	rec := httptest.NewRecorder()
	if got := wrapResponseWriterWithServerTiming(context.Background(), rec); got != http.ResponseWriter(rec) {
		t.Errorf("wrapped without step times: %T", got)
	}
}
