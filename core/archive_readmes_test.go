// Unit tests for the README snapshots in the archive (`archive_readmes.go`)
package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Tests how the README file content is made and read back.
//   - Short text is kept as it is.
//     A UTF-8 byte order mark is removed
//   - Blank text isn't usable
//   - Long text is cut after its last line break within `maxReadmeBytes`,
//     or at a character boundary if there is no line break or it would keep less than half of it, and the truncation line is added
//   - Text over the limit only by blank space at its end loses that space and isn't cut
//   - Text of exactly `maxReadmeBytes` isn't cut
func TestBuildReadmeSnapshotFile(t *testing.T) {
	cases := []struct {
		name       string
		readmeText string
		want       string // File content. Empty if not usable
	}{
		{"short", "# Title\nbody\n", "# Title\nbody\n"},
		{"UTF-8 byte order mark", "\xEF\xBB\xBF# Title\n", "# Title\n"},
		{"blank", " \n\t\n", ""},
		{"exactly the limit", strings.Repeat("a", maxReadmeBytes), strings.Repeat("a", maxReadmeBytes)},
		{"cut at a line break", strings.Repeat(strings.Repeat("x", 99)+"\n", maxReadmeBytes/100+20),
			strings.Repeat(strings.Repeat("x", 99)+"\n", maxReadmeBytes/100) + readmeTruncationLine + "\n"},
		{"cut at a character boundary", strings.Repeat("가", maxReadmeBytes/3+300), strings.Repeat("가", maxReadmeBytes/3) + "\n" + readmeTruncationLine + "\n"},
		{"line break at the start", "\n" + strings.Repeat("x", maxReadmeBytes+1000), "\n" + strings.Repeat("x", maxReadmeBytes-1) + "\n" + readmeTruncationLine + "\n"},
		{"short first line, then one long line", "# Title\n" + strings.Repeat("y", 50000),
			"# Title\n" + strings.Repeat("y", maxReadmeBytes-8) + "\n" + readmeTruncationLine + "\n"},
		{"over the limit only by blank space at the end", strings.Repeat("a", maxReadmeBytes-200) + strings.Repeat(" \n", 500), strings.Repeat("a", maxReadmeBytes-200)},
	}
	for _, c := range cases {
		got, usable := buildReadmeSnapshotFile(c.readmeText)
		if usable != (c.want != "") || string(got) != c.want {
			t.Errorf("%s: got %q (usable %v)", c.name, got, usable)
		}
	}

	// Reading back gives the text without the truncation line, and whether it is truncated
	content, _ := buildReadmeSnapshotFile(strings.Repeat(strings.Repeat("x", 99)+"\n", maxReadmeBytes/100+20))
	if text, isTruncated := parseReadmeSnapshotFile(content); !isTruncated || text != strings.Repeat(strings.Repeat("x", 99)+"\n", maxReadmeBytes/100) {
		t.Errorf("parse truncated file: truncated %v, %d bytes", isTruncated, len(text))
	}
	if text, isTruncated := parseReadmeSnapshotFile([]byte("# Title\n")); isTruncated || text != "# Title\n" {
		t.Errorf("parse whole file: truncated %v, %q", isTruncated, text)
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

// Tests that every path `FetchedReadme.buildSnapshotPath` makes is a clean path, and that `collectReadmeSnapshotsByID` reads it back under the same ID.
// IDs 7 and 105 are checked too, so the zero-padded folder names (`07`, `05`) are covered
func TestFetchedReadmePath(t *testing.T) {
	fetchedReadmes := map[string]FetchedReadme{ // Path for ID 12345 -> fetched README
		"db/readmes/45/12345.md":        {extension: "md", content: []byte("# Title\n")},
		"db/readmes/45/12345":           {content: []byte("# Title\n")},
		"db/readmes/45/12345.no-readme": {isAbsent: true},
	}
	for wantPath, fetched := range fetchedReadmes {
		for _, id := range []int64{12345, 7, 105} {
			path := fetched.buildSnapshotPath(id)
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
	ctx := context.Background()
	repo, _ := startFakeTreeAPI(t)
	snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, "c1", "db/readmes/45")
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
	ctx := context.Background()
	repo, requests := startFakeTreeAPI(t)

	paths, err := readReadmeSnapshotPaths(ctx, repo, "c1", 12345)
	if want := []string{"db/readmes/45/12345.md"}; err != nil || !slices.Equal(paths, want) || requests.Load() != 1 {
		t.Errorf("item 12345: %q, %v, requests %d", paths, err, requests.Load())
	}

	if paths, err := readReadmeSnapshotPaths(ctx, repo, "c1", 7); err != nil || len(paths) != 0 || requests.Load() != 2 {
		t.Errorf("item with no folder: %q, %v, requests %d", paths, err, requests.Load())
	}
}

// Tests that a README call cut at its deadline counts as running out of time, not as a failure of GitHub.
// Counting it as a failure would stop the sync step as if GitHub were down (`errReadmesAllFailed`).
//   - A call cut at the deadline of `ctx`
//   - A call cut at `fetchEndBy`, which comes before the deadline of `ctx`.
//     Its first query answers in time, and the next one waits past `fetchEndBy`.
//     The call ends at `fetchEndBy`, before the time limit of that query, so the time kept for the commit stays free
func TestReadmeCallCutAtDeadlineIsOutOfTime(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // The server notices a closed connection only after the body is read
		select {                    // Waits until the caller gives up
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	cfg := &EnvConfig{GitHubAPI: slow.URL, DBRepoAccessToken: "test-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	fetched, stopErr := fetchReadmesInParallel(ctx, cfg, []StarItem{{ID: 1, NodeID: "R_1"}}, time.Now().Add(time.Hour))
	if len(fetched) != 0 || !errors.Is(stopErr, errReadmesOutOfTime) {
		t.Errorf("cut at the deadline of ctx: %v, %v", fetched, stopErr)
	}

	var queries atomic.Int32
	listingThenSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if queries.Add(1) == 1 {
			time.Sleep(800 * time.Millisecond) // The next query starts after this, so its time limit ends well after `fetchEndBy`
			fmt.Fprint(w, `{"data":{"nodes":[{"id":"R_1","github":null,"root":{"entries":[{"name":"README.md","type":"blob","mode":33188}]}}]}}`)
			return
		}
		io.Copy(io.Discard, r.Body) // The server notices a closed connection only after the body is read
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(listingThenSlow.Close)
	savedClient := httpClient
	httpClient = &http.Client{Timeout: 2 * time.Second} // Stands in for `githubTimeout`, so a call can start with `fetchEndBy` close
	t.Cleanup(func() { httpClient = savedClient })
	cfg = &EnvConfig{GitHubAPI: listingThenSlow.URL, DBRepoAccessToken: "test-token"}
	start := time.Now()
	fetched, stopErr = fetchReadmesInParallel(context.Background(), cfg, []StarItem{{ID: 1, NodeID: "R_1"}}, start.Add(2100*time.Millisecond))
	took := time.Since(start) // Cut at 2.1 seconds. Without the cut, the second query would wait until 2.8 seconds or later
	if len(fetched) != 0 || !errors.Is(stopErr, errReadmesOutOfTime) || queries.Load() != 2 || took > 2500*time.Millisecond {
		t.Errorf("cut at fetchEndBy: %v, %v, queries %d, after %v", fetched, stopErr, queries.Load(), took)
	}
}

// Tests that a single request that hits `githubTimeout` counts as a failed call, not as running out of time.
// Both give an error that matches `context.DeadlineExceeded` (measured), so running out of time is told by the context
func TestReadmeCallTimeoutIsFailure(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // The server notices a closed connection only after the body is read
		select {                    // Waits until the caller gives up
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	savedClient := httpClient
	httpClient = &http.Client{Timeout: 100 * time.Millisecond} // Stands in for `githubTimeout`, which is too long for a test
	t.Cleanup(func() { httpClient = savedClient })
	cfg := &EnvConfig{GitHubAPI: slow.URL, DBRepoAccessToken: "test-token"}
	fetched, stopErr := fetchReadmesInParallel(context.Background(), cfg, []StarItem{{ID: 1, NodeID: "R_1"}}, time.Now().Add(time.Hour))
	if len(fetched) != 0 || !errors.Is(stopErr, errReadmesAllFailed) {
		t.Errorf("one slow request: %v, %v", fetched, stopErr)
	}
}

// Tests what `planReadmeSnapshotUpdate` writes and deletes for one item.
//   - A new README is written and the absent marker next to it is deleted
//   - The same content at the same path writes nothing
//   - Changed content at the same path is written again, and nothing is deleted
//   - A README under another extension replaces the old file
//   - An absent README keeps a saved README, and deletes only an absent marker next to it
//   - An absent README of an item with no saved README writes the absent marker, once
func TestPlanReadmeSnapshotUpdate(t *testing.T) {
	fetched := FetchedReadme{extension: "md", content: []byte("# Title\n")}
	absent := FetchedReadme{isAbsent: true}
	savedMarkdown := readmeSnapshot{path: "db/readmes/45/12345.md", blobSHA: computeGitBlobSHA([]byte("# Title\n"))}
	oldMarkdown := readmeSnapshot{path: "db/readmes/45/12345.md", blobSHA: computeGitBlobSHA([]byte("# Old\n"))}
	savedMarker := readmeSnapshot{path: "db/readmes/45/12345.no-readme", blobSHA: computeGitBlobSHA(nil), isAbsent: true}
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
