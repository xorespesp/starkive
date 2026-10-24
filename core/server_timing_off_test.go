//go:build !timing

package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests that a build without the `timing` tag sends no `Server-Timing` header, even when the handler times steps
func TestServeAPIRequestSendsNoServerTiming(t *testing.T) {
	rec := httptest.NewRecorder()
	ServeAPIRequest(rec, httptest.NewRequest("GET", "http://example.test/api/archive-star-items", nil), func(w http.ResponseWriter, r *http.Request) error {
		beginServerTimingStep(r.Context(), "w-head").end()
		WriteAPIJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
		return nil
	})
	if got := rec.Result().Header.Get("Server-Timing"); got != "" || rec.Code != http.StatusOK {
		t.Errorf("answer %d, Server-Timing %q", rec.Code, got)
	}
}
