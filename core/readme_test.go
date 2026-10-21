// Unit tests for the README snapshot files (`readme.go`)
package core

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests how the README file content is made and read back.
//   - Short text is kept as it is. A UTF-8 byte order mark is removed, and UTF-16 with a byte order mark is turned into UTF-8
//   - Blank text isn't usable. Text with a NUL byte and text that isn't UTF-8 (an older encoding) are kept as bytes
//   - Long text is cut after its last line break within `maxReadmeBytes`,
//     or at a character boundary if that line break would keep less than half of it, and the cut line is added
//   - Text over the limit only by blank space at its end loses that space and isn't cut
//   - Text of exactly `maxReadmeBytes` isn't cut
func TestBuildReadmeSnapshotFile(t *testing.T) {
	cases := []struct {
		name        string
		sourceBytes string
		want        string // File content. Empty if not usable
	}{
		{"short", "# Title\nbody\n", "# Title\nbody\n"},
		{"UTF-8 byte order mark", "\xEF\xBB\xBF# Title\n", "# Title\n"},
		{"UTF-16 little endian", "\xFF\xFE#\x00 \x00h\x00i\x00", "# hi"},
		{"UTF-16 big endian", "\xFE\xFF\x00#\x00 \x00h\x00i", "# hi"},
		{"blank", " \n\t\n", ""},
		{"NUL byte", "a\x00b", "a\x00b"},
		{"not UTF-8 (EUC-KR)", "\xC7\xD1\xB1\xDB\n", "\xC7\xD1\xB1\xDB\n"},
		{"not UTF-8, cut at a line break", strings.Repeat("\xC7\xD1\xB1\xDB\n", 2000),
			strings.Repeat("\xC7\xD1\xB1\xDB\n", 1638) + readmeCutLine + "\n"},
		{"UTF-16 without a byte order mark, cut after the NUL of a line break", strings.Repeat("x\x00\n\x00", 3000),
			strings.Repeat("x\x00\n\x00", maxReadmeBytes/4) + "\n" + readmeCutLine + "\n"},
		{"exactly the limit", strings.Repeat("a", maxReadmeBytes), strings.Repeat("a", maxReadmeBytes)},
		{"cut at a line break", strings.Repeat(strings.Repeat("x", 99)+"\n", 100),
			strings.Repeat(strings.Repeat("x", 99)+"\n", 81) + readmeCutLine + "\n"},
		{"cut at a character boundary", strings.Repeat("가", 3000), strings.Repeat("가", 2730) + "\n" + readmeCutLine + "\n"},
		{"line break at the start", "\n" + strings.Repeat("x", 9000), "\n" + strings.Repeat("x", maxReadmeBytes-1) + "\n" + readmeCutLine + "\n"},
		{"short first line, then one long line", "# Title\n" + strings.Repeat("y", 50000),
			"# Title\n" + strings.Repeat("y", maxReadmeBytes-8) + "\n" + readmeCutLine + "\n"},
		{"over the limit only by blank space at the end", strings.Repeat("a", 8000) + strings.Repeat(" \n", 500), strings.Repeat("a", 8000)},
	}
	for _, c := range cases {
		got, usable := buildReadmeSnapshotFile([]byte(c.sourceBytes))
		if usable != (c.want != "") || string(got) != c.want {
			t.Errorf("%s: got %q (usable %v)", c.name, got, usable)
		}
	}

	// Reading back gives the text without the cut line, and whether it was cut
	content, _ := buildReadmeSnapshotFile([]byte(strings.Repeat(strings.Repeat("x", 99)+"\n", 100)))
	if text, isCut := parseReadmeSnapshotFile(content); !isCut || text != strings.Repeat(strings.Repeat("x", 99)+"\n", 81) {
		t.Errorf("parse cut file: cut %v, %d bytes", isCut, len(text))
	}
	if text, isCut := parseReadmeSnapshotFile([]byte("# Title\n")); isCut || text != "# Title\n" {
		t.Errorf("parse whole file: cut %v, %q", isCut, text)
	}
}

// Tests which paths under `ReadmesDBDir` count as README snapshots.
// Only `<ID>`, `<ID>.<extension>` and `<ID>.no-readme` in the folder of that ID count.
// A wrong folder, a leading zero, a disallowed extension and a deeper path are ignored
func TestCollectReadmeSnapshotsByID(t *testing.T) {
	blobSHAByPath := map[string]string{
		"db/readmes/45/12345":           "a",
		"db/readmes/45/12345.no-readme": "b",
		"db/readmes/45/345.md":          "c", // Another item in the same folder
		"db/readmes/46/12345.md":        "d", // Wrong folder
		"db/readmes/45/012345.md":       "e", // Leading zero
		"db/readmes/45/12345.Md":        "f", // Upper case extension, which `readmeExtensionPattern` doesn't allow
		"db/readmes/45/x/12345.md":      "g", // Deeper path
		"db/readmes/45/notes.txt":       "h",
	}
	got := []string{}
	for id, snapshots := range collectReadmeSnapshotsByID(blobSHAByPath) {
		for _, snapshot := range snapshots {
			got = append(got, fmt.Sprintf("%d %s %s %v", id, snapshot.path, snapshot.blobSHA, snapshot.isAbsent))
		}
	}
	slices.Sort(got)
	want := []string{
		"12345 db/readmes/45/12345 a false",
		"12345 db/readmes/45/12345.no-readme b true",
		"345 db/readmes/45/345.md c false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("grouped = %q", got)
	}
}

// Tests that every path `FetchedReadme.buildPath` makes is a clean path, and that `collectReadmeSnapshotsByID` reads it back under the same ID.
// IDs 7 and 105 are checked too, so the zero-padded folder names (`07`, `05`) are covered
func TestFetchedReadmePath(t *testing.T) {
	fetchedReadmes := map[string]FetchedReadme{ // Path for ID 12345 -> fetched README
		"db/readmes/45/12345.md":        {extension: "md", content: []byte("# Title\n")},
		"db/readmes/45/12345":           {content: []byte("# Title\n")},
		"db/readmes/45/12345.no-readme": {isAbsent: true},
	}
	for wantPath, fetched := range fetchedReadmes {
		for _, id := range []int64{12345, 7, 105} {
			path := fetched.buildPath(id)
			if id == 12345 && path != wantPath {
				t.Errorf("path for %d: got %q, want %q", id, path, wantPath)
			}
			snapshots := collectReadmeSnapshotsByID(map[string]string{path: "sha"})[id]
			if !isValidRepoPath(path) || len(snapshots) != 1 || snapshots[0].isAbsent != fetched.isAbsent {
				t.Errorf("%q isn't read back as a snapshot of %d: %v", path, id, snapshots)
			}
		}
	}
}

// Tests that `readReadmeSnapshotsInFolder` keeps only the README snapshots of a folder listing.
// The file that isn't a README and the file in a subfolder of the listing are left out
func TestReadReadmeSnapshotsInFolder(t *testing.T) {
	cfg, _ := startFakeTreeAPI(t)
	snapshotsByID, err := readReadmeSnapshotsInFolder(cfg, "c1", "db/readmes/45")
	want := map[int64][]readmeSnapshot{
		12345: {{path: "db/readmes/45/12345.md", blobSHA: "a"}},
		345:   {{path: "db/readmes/45/345.no-readme", blobSHA: "b", isAbsent: true}},
	}
	if err != nil || fmt.Sprint(snapshotsByID) != fmt.Sprint(want) {
		t.Errorf("snapshots: %v, %v", snapshotsByID, err)
	}
}

// Tests that `readReadmeSnapshotPaths` lists the folder of the item once, and returns only the paths of that item.
//   - Item 12345 shares its folder with item 345 and other files, and gets only its own README
//   - An item whose folder doesn't exist gets no path
func TestReadReadmeSnapshotPaths(t *testing.T) {
	cfg, requests := startFakeTreeAPI(t)

	paths, err := readReadmeSnapshotPaths(cfg, "c1", 12345)
	if want := []string{"db/readmes/45/12345.md"}; err != nil || !slices.Equal(paths, want) || requests.Load() != 1 {
		t.Errorf("item 12345: %q, %v, requests %d", paths, err, requests.Load())
	}

	if paths, err := readReadmeSnapshotPaths(cfg, "c1", 7); err != nil || len(paths) != 0 || requests.Load() != 2 {
		t.Errorf("item with no folder: %q, %v, requests %d", paths, err, requests.Load())
	}
}

// Tests how `FetchReadmeByRepoID` reads each kind of GitHub answer, against a test server.
//   - A README is decoded, and its extension is kept in lower case only if it is allowed
//   - 404, 451 and a blank README mean the README is absent. Any other README is kept as bytes
//   - 429 and a 403 rate limit answer are errors that wrap `errGitHubRateLimited`
//   - A 403 that isn't a rate limit is an error that doesn't wrap it, so nothing is saved
//   - 5xx and a timeout are other errors, which say nothing about the README
//   - A README over 1MB comes with empty content, so its start is read in raw format, cut to `maxReadmeBytes`
//   - Both requests of a large README share one deadline, and no request is sent after it
func TestFetchReadmeByRepoID(t *testing.T) {
	requestsOf16 := &atomic.Int32{}                                   // Requests for README 16. The server answers in its own goroutine, so the count is atomic
	largeReadme := strings.Repeat(strings.Repeat("x", 99)+"\n", 2000) // 200KB
	answers := map[string]func(w http.ResponseWriter, r *http.Request){
		"/repositories/1/readme": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"name":"README.md","size":6,"content":%q}`, base64.StdEncoding.EncodeToString([]byte("# One\n")))
		},
		"/repositories/2/readme": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"name":"README.RST","size":5,"content":%q}`, base64.StdEncoding.EncodeToString([]byte("Two\n\n")))
		},
		"/repositories/3/readme": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"name":"README.old-md","size":6,"content":%q}`, base64.StdEncoding.EncodeToString([]byte("Three\n")))
		},
		"/repositories/4/readme": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"name":"README.md","size":3,"content":%q}`, base64.StdEncoding.EncodeToString([]byte("a\x00b")))
		},
		"/repositories/5/readme": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		},
		"/repositories/6/readme": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Repository access blocked"}`, http.StatusUnavailableForLegalReasons)
		},
		"/repositories/7/readme": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Resource not accessible"}`, http.StatusForbidden)
		},
		"/repositories/8/readme": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		},
		"/repositories/13/readme": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, `{"message":"Forbidden"}`, http.StatusForbidden)
		},
		"/repositories/14/readme": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusForbidden)
		},
		"/repositories/15/readme": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"name":"README","size":0,"content":""}`)
		},
		"/repositories/9/readme": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"slow down"}`, http.StatusTooManyRequests)
		},
		"/repositories/10/readme": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		},
		"/repositories/11/readme": func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond) // Longer than the time limit the test gives
			fmt.Fprint(w, `{"name":"README.md","size":0,"content":""}`)
		},
		"/repositories/12/readme": func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept") == "application/vnd.github.raw+json" {
				fmt.Fprint(w, largeReadme)
				return
			}
			fmt.Fprintf(w, `{"name":"README.md","size":%d,"content":""}`, len(largeReadme))
		},
		"/repositories/16/readme": func(w http.ResponseWriter, r *http.Request) {
			requestsOf16.Add(1)
			time.Sleep(150 * time.Millisecond) // Each answer fits in the deadline the test gives, but the two together don't
			if r.Header.Get("Accept") == "application/vnd.github.raw+json" {
				fmt.Fprint(w, largeReadme)
				return
			}
			fmt.Fprintf(w, `{"name":"README.md","size":%d,"content":""}`, len(largeReadme))
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer, ok := answers[r.URL.Path]
		if !ok {
			http.Error(w, `{"message":"unexpected path"}`, http.StatusTeapot)
			return
		}
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := &Config{GitHubAPI: server.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}

	cases := []struct {
		id            int64
		want          FetchedReadme
		wantErr       bool // Any error
		isRateLimited bool // The error wraps `errGitHubRateLimited`
	}{
		{1, FetchedReadme{extension: "md", content: []byte("# One\n")}, false, false},
		{2, FetchedReadme{extension: "rst", content: []byte("Two\n\n")}, false, false},
		{3, FetchedReadme{content: []byte("Three\n")}, false, false},                 // `old-md` isn't allowed, so it has no extension
		{4, FetchedReadme{extension: "md", content: []byte("a\x00b")}, false, false}, // Kept as bytes, like any README that isn't blank
		{5, FetchedReadme{isAbsent: true}, false, false},
		{6, FetchedReadme{isAbsent: true}, false, false},
		{7, FetchedReadme{}, true, false},
		{8, FetchedReadme{}, true, true},                  // 403 with no requests left
		{13, FetchedReadme{}, true, true},                 // 403 with `Retry-After`
		{14, FetchedReadme{}, true, true},                 // 403 that says "rate limit"
		{9, FetchedReadme{}, true, true},                  // 429
		{15, FetchedReadme{isAbsent: true}, false, false}, // An empty README, with no dot in its name, isn't read again in raw format
		{10, FetchedReadme{}, true, false},
		{11, FetchedReadme{}, true, false},
	}
	for _, c := range cases {
		got, err := FetchReadmeByRepoID(cfg, c.id, time.Now().Add(100*time.Millisecond))
		if (err != nil) != c.wantErr || errors.Is(err, errGitHubRateLimited) != c.isRateLimited || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("README of %d: %+v, %v", c.id, got, err)
		}
	}

	got, err := FetchReadmeByRepoID(cfg, 12, time.Now().Add(githubTimeout))
	wantContent, _ := buildReadmeSnapshotFile([]byte(largeReadme))
	if err != nil || got.isAbsent || got.extension != "md" || !bytes.Equal(got.content, wantContent) {
		t.Errorf("large README: %v, %d bytes", err, len(got.content))
	}

	if _, err := FetchReadmeByRepoID(cfg, 16, time.Now().Add(250*time.Millisecond)); err == nil || requestsOf16.Load() != 2 {
		t.Errorf("large README past one shared deadline: %v, requests %d", err, requestsOf16.Load())
	}
	if _, err := FetchReadmeByRepoID(cfg, 1, time.Now().Add(-time.Second)); !errors.Is(err, errDeadlinePassed) {
		t.Errorf("deadline already passed: %v", err)
	}
}

// Tests what `planReadmeSnapshotUpdate` writes and deletes for one item.
//   - A new README is written and the absent marker next to it is deleted
//   - The same content at the same path writes nothing
//   - A README under another extension replaces the old file
//   - An absent README keeps a saved README, and deletes only an absent marker next to it
//   - An absent README of an item with no saved README writes the absent marker, once
func TestPlanReadmeSnapshotUpdate(t *testing.T) {
	fetched := FetchedReadme{extension: "md", content: []byte("# Title\n")}
	absent := FetchedReadme{isAbsent: true}
	savedMarkdown := readmeSnapshot{path: "db/readmes/45/12345.md", blobSHA: gitBlobSHA([]byte("# Title\n"))}
	oldMarkdown := readmeSnapshot{path: "db/readmes/45/12345.md", blobSHA: gitBlobSHA([]byte("# Old\n"))}
	savedMarker := readmeSnapshot{path: "db/readmes/45/12345.no-readme", blobSHA: gitBlobSHA(nil), isAbsent: true}
	savedText := readmeSnapshot{path: "db/readmes/45/12345.rst", blobSHA: "old"}

	cases := []struct {
		name        string
		fetched     FetchedReadme
		saved       []readmeSnapshot
		wantWritten []string
		wantDeleted []string
	}{
		{"new README", fetched, []readmeSnapshot{savedMarker}, []string{savedMarkdown.path}, []string{savedMarker.path}},
		{"same content", fetched, []readmeSnapshot{savedMarkdown}, nil, nil},
		{"changed content", fetched, []readmeSnapshot{oldMarkdown}, []string{savedMarkdown.path}, nil},
		{"other extension", fetched, []readmeSnapshot{savedText}, []string{savedMarkdown.path}, []string{savedText.path}},
		{"absent with a saved README", absent, []readmeSnapshot{savedMarkdown}, nil, nil},
		{"absent with a saved README and a marker", absent, []readmeSnapshot{savedMarkdown, savedMarker}, nil, []string{savedMarker.path}},
		{"absent with nothing saved", absent, nil, []string{savedMarker.path}, nil},
		{"absent with the marker saved", absent, []readmeSnapshot{savedMarker}, nil, nil},
	}
	for _, c := range cases {
		changes := FileChangesCommit{}
		planReadmeSnapshotUpdate(12345, c.fetched, c.saved, &changes)
		written := []string{}
		for path := range changes.FilesToWrite {
			written = append(written, path)
		}
		// `fmt.Sprint` prints a `nil` slice and an empty one the same way, so "nothing" compares equal either way
		if fmt.Sprint(written) != fmt.Sprint(c.wantWritten) || fmt.Sprint(changes.FilesToDelete) != fmt.Sprint(c.wantDeleted) {
			t.Errorf("%s: written %q, deleted %q", c.name, written, changes.FilesToDelete)
		}
	}
}
