// Integration tests for the archive file answers that the browser may keep (`ETag` and 304), against the fake GitHub
package test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/xorespesp/starkive/api"
)

// Sends a GET to an archive file API with the session cookie, and optionally `If-None-Match` and `Accept-Encoding: gzip`
func getArchiveFile(h http.HandlerFunc, target string, cookie string, etag string, gzip bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "http://example.test"+target, nil)
	req.Header.Set("Cookie", cookie)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if gzip {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// Tests that the archive files answer 304 to the browser's `ETag` while they don't change.
//   - The first answer has an `ETag` and `Cache-Control: private, no-cache` (kept only by the browser, checked every time)
//   - The same `ETag` gets 304 with no body. For the items, no shard is read (GitHub answers the listing with 304)
//   - The gzip and plain answers have different `ETag`s
//   - After a write, the items answer again with a new `ETag`, and the star lists still answer 304
//   - An `ETag` of another build isn't used
func TestArchiveFilesAnswerNotModified(t *testing.T) {
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "0123456789abcdef0123")
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.files["db/repo-meta.json"] = "[\n" + `{"id":1,"name":"a/one"}` + "\n]\n"
	cookie := login(t)
	files := map[string]http.HandlerFunc{
		"/api/archive-star-items": api.HandleArchiveStarItems,
		"/api/archive-star-lists": api.HandleArchiveStarLists,
		"/api/archive-repo-meta":  api.HandleArchiveRepoMeta,
	}
	etags := map[string]string{}
	for target, h := range files {
		first := getArchiveFile(h, target, cookie, "", false)
		etag := first.Header().Get("ETag")
		if first.Code != 200 || etag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("%s first: %d, ETag %q, Cache-Control %q", target, first.Code, etag, first.Header().Get("Cache-Control"))
		}
		etags[target] = etag
		gh.mu.Lock()
		readsBefore, blobsBefore := len(gh.fileReads), gh.blobReads
		gh.mu.Unlock()
		again := getArchiveFile(h, target, cookie, etag, false)
		gh.mu.Lock()
		shardReads := 0
		for _, path := range gh.fileReads[readsBefore:] {
			if strings.HasPrefix(path, "db/star-items/") {
				shardReads++
			}
		}
		blobReads := gh.blobReads - blobsBefore
		gh.mu.Unlock()
		if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("ETag") != etag || shardReads != 0 || blobReads != 0 {
			t.Errorf("%s again: %d, body %d bytes, ETag %q, shard reads %d, blob reads %d", target, again.Code, again.Body.Len(), again.Header().Get("ETag"), shardReads, blobReads)
		}
		if zipped := getArchiveFile(h, target, cookie, etag, true); zipped.Code != 200 || zipped.Header().Get("ETag") == etag {
			t.Errorf("%s gzip: %d, ETag %q", target, zipped.Code, zipped.Header().Get("ETag"))
		}
	}

	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"changed"}`, cookie); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	items := getArchiveFile(api.HandleArchiveStarItems, "/api/archive-star-items", cookie, etags["/api/archive-star-items"], false)
	if items.Code != 200 || items.Header().Get("ETag") == etags["/api/archive-star-items"] || !strings.Contains(items.Body.String(), `"note":"changed"`) {
		t.Errorf("items after a write: %d, ETag %q", items.Code, items.Header().Get("ETag"))
	}
	if lists := getArchiveFile(api.HandleArchiveStarLists, "/api/archive-star-lists", cookie, etags["/api/archive-star-lists"], false); lists.Code != http.StatusNotModified {
		t.Errorf("star lists after an item write: %d", lists.Code)
	}

	t.Setenv("VERCEL_GIT_COMMIT_SHA", "fedcba9876543210fedc") // A new build
	if lists := getArchiveFile(api.HandleArchiveStarLists, "/api/archive-star-lists", cookie, etags["/api/archive-star-lists"], false); lists.Code != 200 {
		t.Errorf("star lists with an ETag of another build: %d", lists.Code)
	}
}

// Tests that without a build commit (dev-server), the archive files have no `ETag` and aren't kept (`no-store`).
// Signing out tells the browser to clear what it kept (`Clear-Site-Data`)
func TestArchiveFilesWithoutABuildAreNotKept(t *testing.T) {
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "")
	setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	rec := getArchiveFile(api.HandleArchiveStarItems, "/api/archive-star-items", cookie, "", false)
	if rec.Code != 200 || rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("items: %d, ETag %q, Cache-Control %q", rec.Code, rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"))
	}
	if rec := call(api.HandleLoginSession, "DELETE", "/api/login-session", "", cookie); rec.Code != 200 || rec.Header().Get("Clear-Site-Data") != `"cache"` {
		t.Errorf("sign out: %d, Clear-Site-Data %q", rec.Code, rec.Header().Get("Clear-Site-Data"))
	}
}

// Tests that a request without a session gets 401 even with an `ETag` that matches, so a kept answer is never confirmed without signing in
func TestArchiveFilesNeedSignInBeforeNotModified(t *testing.T) {
	t.Setenv("VERCEL_GIT_COMMIT_SHA", "0123456789abcdef0123")
	setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	etag := getArchiveFile(api.HandleArchiveStarLists, "/api/archive-star-lists", cookie, "", false).Header().Get("ETag")
	if rec := getArchiveFile(api.HandleArchiveStarLists, "/api/archive-star-lists", "", etag, false); rec.Code != http.StatusUnauthorized || etag == "" {
		t.Errorf("without a session: %d, ETag %q", rec.Code, etag)
	}
}
