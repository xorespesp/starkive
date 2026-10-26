// Unit tests for the data files of the archive (`data_file.go`)
package core

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// Tests that asking for a file not read at the commit panics, instead of looking like a file that doesn't exist.
// A file that was read but doesn't exist (`nil`) is an empty list, and `DBRepoFilesAtCommit.HasFile` tells it apart.
// A file that holds `null` is an empty list too
func TestDataFileRefusesFileNotRead(t *testing.T) {
	notRead := &DBRepoFilesAtCommit{commitSHA: "c1", contents: map[string][]byte{StarListsDBPath: []byte("[]\n")}}
	var repoMeta []RepoMeta
	for name, ask := range map[string]func(){
		"decodeForReading": func() { repoMetaDataFile.decodeForReading(notRead) },
		"decodeForWriting": func() { repoMetaDataFile.decodeForWriting(notRead, &repoMeta) },
		"HasFile":          func() { notRead.HasFile(RepoMetaDBPath) },
		"empty value":      func() { (&DBRepoFilesAtCommit{}).FileContent(RepoMetaDBPath) },
	} {
		if recovered := catchPanic(ask); recovered == nil || !strings.Contains(fmt.Sprint(recovered), RepoMetaDBPath) {
			t.Errorf("%s, file not read: panic %v", name, recovered)
		}
	}
	items, err := repoMetaDataFile.decodeForReading(&DBRepoFilesAtCommit{contents: map[string][]byte{RepoMetaDBPath: nil}})
	if err != nil || items == nil || len(items) != 0 || (&DBRepoFilesAtCommit{contents: map[string][]byte{RepoMetaDBPath: nil}}).HasFile(RepoMetaDBPath) {
		t.Errorf("missing file: %v, %v", items, err)
	}
	items, err = repoMetaDataFile.decodeForReading(&DBRepoFilesAtCommit{contents: map[string][]byte{RepoMetaDBPath: []byte("null")}})
	if err != nil || items == nil || len(items) != 0 || !(&DBRepoFilesAtCommit{contents: map[string][]byte{RepoMetaDBPath: []byte("null")}}).HasFile(RepoMetaDBPath) {
		t.Errorf("null file: %v, %v", items, err)
	}
}

// Tests when `trackedDataFile.addIfChanged` writes the file.
//   - Not when the items are the same, also if the file had another layout or didn't exist
//   - When the tracked variable changed, also if the change replaced the slice (`append`)
func TestTrackedDataFileWritesOnlyChangedItems(t *testing.T) {
	standard := "[\n{\"id\":100,\"name\":\"C\",\"description\":\"\"}\n]\n"
	indented := "[\n  {\"id\": 100, \"name\": \"C\", \"description\": \"\"}\n]\n"
	for _, c := range []struct {
		name      string
		content   []byte
		change    func(starLists *[]StarList)
		wantWrite bool
	}{
		{"same", []byte(standard), func(starLists *[]StarList) {}, false},
		{"other layout, same items", []byte(indented), func(starLists *[]StarList) {}, false},
		{"missing file, untouched", nil, func(starLists *[]StarList) {}, false},
		{"edited in place", []byte(standard), func(starLists *[]StarList) { (*starLists)[0].Name = "Go" }, true},
		{"slice replaced", []byte(standard), func(starLists *[]StarList) { *starLists = append(*starLists, StarList{ID: 200, Name: "OS"}) }, true},
	} {
		var starLists []StarList
		tracked, err := starListsDataFile.decodeForWriting(&DBRepoFilesAtCommit{contents: map[string][]byte{StarListsDBPath: c.content}}, &starLists)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		c.change(&starLists)
		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}}
		if err := tracked.addIfChanged(&changes); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		written, wrote := changes.FilesToWrite[StarListsDBPath]
		if wrote != c.wantWrite {
			t.Errorf("%s: wrote %v, want %v", c.name, wrote, c.wantWrite)
		}
		if wrote && !strings.Contains(string(written), `"id":`) {
			t.Errorf("%s: written %q", c.name, written)
		}
	}
}

// Makes the files of the DB repository with every shard: shard index -> content.
// A shard not in `contentByIndex` holds `[]`, and an empty string is a shard that doesn't exist
func newTestShardContents(contentByIndex map[int]string) map[string]string {
	files := map[string]string{}
	for index := range StarItemShardCount {
		content, ok := contentByIndex[index]
		switch {
		case !ok:
			files[BuildStarItemShardPath(index)] = "[]\n"
		case content != "":
			files[BuildStarItemShardPath(index)] = content
		}
	}
	return files
}

// Makes the files read at one commit with every shard, as `newTestShardContents` makes them
func newTestShardFiles(contentByIndex map[int]string) *DBRepoFilesAtCommit {
	files := &DBRepoFilesAtCommit{commitSHA: "c1", contents: map[string][]byte{}}
	contents := newTestShardContents(contentByIndex)
	for index := range StarItemShardCount {
		path := BuildStarItemShardPath(index)
		if content, ok := contents[path]; ok {
			files.contents[path] = []byte(content)
		} else {
			files.contents[path] = nil
		}
	}
	return files
}

// Starts a test server that answers like GitHub for the DB repository `me/archive`, whose branch `main` is at commit `c1`.
// It answers the listing of `StarItemsDBDir` at the branch, file reads at `c1` and at the branch, and blob reads by sha.
//
// Args:
//   - `files`: Path -> content at `c1`. The listing and the blob reads come from these
//   - `branchReads`: Path -> what a file read at the branch gives instead, as if it landed on another commit. An empty string is a 404
//
// Returns:
//   - The DB repository whose requests go to the server (`newTestDBRepo`), and the number of blob reads.
//     The server answers in its own goroutines, so the count is an atomic counter
func startFakeShardAPI(t *testing.T, files map[string]string, branchReads map[string]string) (*DBRepo, *atomic.Int32) {
	blobReads := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.EscapedPath() == "/repos/me/archive/git/trees/refs%2Fheads%2Fmain:"+StarItemsDBDir:
			entries := []string{}
			for path, content := range files {
				if name, inFolder := strings.CutPrefix(path, StarItemsDBDir+"/"); inFolder {
					entries = append(entries, fmt.Sprintf(`{"path":%q,"type":"blob","sha":%q}`, name, computeGitBlobSHA([]byte(content))))
				}
			}
			fmt.Fprintf(w, `{"tree":[%s],"truncated":false}`, strings.Join(entries, ","))
			return
		case strings.HasPrefix(r.URL.Path, "/repos/me/archive/contents/"):
			path := strings.TrimPrefix(r.URL.Path, "/repos/me/archive/contents/")
			content, ok := files[path]
			switch r.URL.Query().Get("ref") {
			case "refs/heads/main":
				if branchContent, isChanged := branchReads[path]; isChanged {
					content, ok = branchContent, branchContent != ""
				}
			case "c1":
			default:
				ok = false
			}
			if ok {
				fmt.Fprint(w, content)
				return
			}
		case strings.HasPrefix(r.URL.Path, "/repos/me/archive/git/blobs/"):
			blobReads.Add(1)
			for _, content := range files {
				if computeGitBlobSHA([]byte(content)) == strings.TrimPrefix(r.URL.Path, "/repos/me/archive/git/blobs/") {
					fmt.Fprint(w, content)
					return
				}
			}
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(server.Close) // `t.Cleanup`: runs when the test ends, like a destructor
	return newTestDBRepo(server.URL), blobReads
}

// Returns the IDs of `items` in order
func listTestStarItemIDs(items []StarItem) []int64 {
	ids := []int64{}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// Tests where items go: shard paths have two digits, an ID goes to `id % StarItemShardCount`,
// and the paths to read cover all shards
func TestStarItemShardPaths(t *testing.T) {
	if got := BuildStarItemShardPath(3); got != "db/star-items/03.json" {
		t.Errorf("path of shard 3: %s", got)
	}
	for id, want := range map[int64]int{1: 1, 8: 0, 15: 7, 1420266966: 6} {
		if got := computeShardIndexOfStarItem(id); got != want {
			t.Errorf("shard of %d: %d, want %d", id, got, want)
		}
	}
	if paths := starItemsDataFile.buildPaths(); len(paths) != StarItemShardCount || paths[0] != "db/star-items/00.json" || paths[7] != "db/star-items/07.json" {
		t.Errorf("all shard paths: %v", paths)
	}
}

// Tests that every way of `shardedStarItemsDataFile` to get the items refuses a shard as a whole when it breaks the layout:
// an ID that isn't positive, an item in another shard, or an ID listed twice.
// The ways are decoding for reading and for writing, and reading from GitHub at a commit and at the branch.
// A missing shard is a 502, not an empty shard.
// Good shards are joined in index order, each in file order
func TestShardedStarItemsDataFileRefusesBrokenShards(t *testing.T) {
	readers := map[string]func(contentByIndex map[int]string) ([]StarItem, error){
		"decode for reading": func(contentByIndex map[int]string) ([]StarItem, error) {
			return starItemsDataFile.decodeForReading(newTestShardFiles(contentByIndex))
		},
		"decode for writing": func(contentByIndex map[int]string) ([]StarItem, error) {
			var items []StarItem
			_, err := starItemsDataFile.decodeShardsForWriting(newTestShardFiles(contentByIndex), listAllStarItemShardIndexes(), &items)
			return items, err
		},
		"read at a commit": func(contentByIndex map[int]string) ([]StarItem, error) {
			repo, _ := startFakeShardAPI(t, newTestShardContents(contentByIndex), nil)
			return starItemsDataFile.readFromDBRepoAtCommit(t.Context(), repo, "c1")
		},
		"read the latest": func(contentByIndex map[int]string) ([]StarItem, error) {
			repo, _ := startFakeShardAPI(t, newTestShardContents(contentByIndex), nil)
			return starItemsDataFile.readLatestFromDBRepo(t.Context(), repo)
		},
	}
	for readerName, read := range readers {
		for name, content := range map[string]string{
			"zero ID":      `[{"id":0,"name":"a/zero"}]`,
			"negative ID":  `[{"id":-7,"name":"a/neg"}]`,
			"wrong shard":  `[{"id":2,"name":"a/two"}]`,
			"listed twice": `[{"id":9,"name":"a/nine"},{"id":9,"name":"a/nine"}]`,
			"not an array": `{"id":1}`,
		} {
			if items, err := read(map[int]string{1: content}); err == nil || items != nil || !strings.Contains(err.Error(), "db/star-items/01.json") {
				t.Errorf("%s, %s: %v, %v", readerName, name, items, err)
			}
		}

		var httpErr *HTTPError
		if _, err := read(map[int]string{3: ""}); !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway || !strings.Contains(err.Error(), "db/star-items/03.json") {
			t.Errorf("%s, missing shard: %v", readerName, err)
		}

		items, err := read(map[int]string{
			0: `[{"id":8,"name":"a/eight"}]`,
			1: `[{"id":9,"name":"a/nine"},{"id":1,"name":"a/one"}]`,
		})
		if ids := listTestStarItemIDs(items); err != nil || !slices.Equal(ids, []int64{8, 9, 1}) {
			t.Errorf("%s, good shards: %v, %v", readerName, ids, err)
		}
	}
}

// Tests that `trackedShardedStarItemsDataFile.addIfChanged` adds only the shards whose items changed.
//   - Nothing is added when nothing changed, even for a shard read with another layout
//   - A changed item rewrites only its shard
//   - An added and a removed item rewrite their shards, also when the change replaces the slice
//   - An ID that isn't positive or appears twice is refused, and nothing is added
func TestTrackedShardedStarItemsDataFileAddsOnlyChangedShards(t *testing.T) {
	var items []StarItem
	tracked, err := starItemsDataFile.decodeShardsForWriting(newTestShardFiles(map[int]string{
		1: "[\n  {\"id\":1,\"node_id\":\"\",\"name\":\"a/one\",\"note\":\"\",\"star_list_ids\":[],\"starred_at\":\"\"}\n]\n", // Another layout
		2: "[\n{\"id\":2,\"name\":\"b/two\",\"note\":\"\",\"star_list_ids\":[],\"starred_at\":\"\"}\n]\n",
	}), listAllStarItemShardIndexes(), &items)
	if err != nil {
		t.Fatal(err)
	}
	addChanged := func() map[string][]byte {
		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}}
		if err := tracked.addIfChanged(&changes); err != nil {
			t.Fatal(err)
		}
		return changes.FilesToWrite
	}

	if files := addChanged(); len(files) != 0 {
		t.Errorf("no change: %q", files)
	}

	items[1].Note = "changed"
	if files := addChanged(); len(files) != 1 || !strings.Contains(string(files[BuildStarItemShardPath(2)]), `"note":"changed"`) {
		t.Errorf("changed note: %q", files)
	}

	items = append([]StarItem{{ID: 10, Name: "c/ten"}}, items[1:]...) // Removes item 1, and adds item 10 to shard 2
	if files := addChanged(); len(files) != 2 || string(files[BuildStarItemShardPath(1)]) != "[]\n" || !strings.Contains(string(files[BuildStarItemShardPath(2)]), `"id":10`) {
		t.Errorf("added and removed: %q", files)
	}

	for name, change := range map[string][]StarItem{
		"listed twice": append(slices.Clone(items), items[0]),
		"zero ID":      append(slices.Clone(items), StarItem{ID: 0}),
	} {
		items = change
		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}}
		if err := tracked.addIfChanged(&changes); err == nil || len(changes.FilesToWrite) != 0 {
			t.Errorf("%s: %v, added %q", name, err, changes.FilesToWrite)
		}
	}
}

// Tests a write that read only one shard.
//   - The items are only the ones of that shard
//   - An item that belongs to a shard that wasn't read is refused, and nothing is added,
//     so a shard can't be replaced by only the new items
//   - A change of its own item rewrites only that shard
func TestTrackedShardedStarItemsDataFileWritesOnlyTheShardsRead(t *testing.T) {
	var items []StarItem
	tracked, err := starItemsDataFile.decodeShardsForWriting(newTestShardFiles(map[int]string{
		1: `[{"id":9,"name":"a/nine"},{"id":1,"name":"a/one"}]`,
		2: `[{"id":2,"name":"b/two"}]`,
	}), []int{1}, &items)
	if ids := listTestStarItemIDs(items); err != nil || !slices.Equal(ids, []int64{9, 1}) {
		t.Fatalf("items of shard 1: %v, %v", ids, err)
	}

	withOtherShard := append(slices.Clone(items), StarItem{ID: 3, Name: "c/three"})
	items = withOtherShard
	changes := FileChangesCommit{FilesToWrite: map[string][]byte{}}
	if err := tracked.addIfChanged(&changes); err == nil || !strings.Contains(err.Error(), BuildStarItemShardPath(3)) || len(changes.FilesToWrite) != 0 {
		t.Errorf("item of a shard not read: %v, added %q", err, changes.FilesToWrite)
	}

	items = withOtherShard[:2]
	items[0].Note = "changed"
	changes = FileChangesCommit{FilesToWrite: map[string][]byte{}}
	if err := tracked.addIfChanged(&changes); err != nil || len(changes.FilesToWrite) != 1 || !strings.Contains(string(changes.FilesToWrite[BuildStarItemShardPath(1)]), `"note":"changed"`) {
		t.Errorf("change in the shard read: %v, added %q", err, changes.FilesToWrite)
	}
}

// Tests that a change which edits the star list IDs of an item in place is still written.
// The items share their `StarListIDs` arrays with the shards as decoded, so each shard as read must be encoded before the change
func TestTrackedShardedStarItemsDataFileSeesAChangeMadeInPlace(t *testing.T) {
	var items []StarItem
	tracked, err := starItemsDataFile.decodeShardsForWriting(newTestShardFiles(map[int]string{
		1: "[\n{\"id\":1,\"node_id\":\"\",\"name\":\"a/one\",\"note\":\"\",\"star_list_ids\":[100,200],\"starred_at\":\"\"}\n]\n",
	}), listAllStarItemShardIndexes(), &items)
	if err != nil {
		t.Fatal(err)
	}
	items[0].StarListIDs[0] = 300 // In place
	changes := FileChangesCommit{FilesToWrite: map[string][]byte{}}
	if err := tracked.addIfChanged(&changes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changes.FilesToWrite[BuildStarItemShardPath(1)]), `"star_list_ids":[300,200]`) {
		t.Errorf("added: %q", changes.FilesToWrite)
	}
}

// Tests that reading the shards at the branch gives the items of the commit that the listing shows.
//   - When every read matches the listing, no blob is read
//   - A read that shows another commit, or gives a passing 404, is read again by the listed blob
//   - A shard missing from the listing is a 502, even if its read gave content
//   - Reading at a commit reads that commit, not the branch
func TestShardedStarItemsDataFileReadsTheShardsOfOneCommit(t *testing.T) {
	ctx := t.Context()
	files := newTestShardContents(map[int]string{
		1: `[{"id":9,"name":"a/nine","note":"new"}]`,
		2: `[{"id":2,"name":"b/two"}]`,
	})
	shard1, shard2, shard3 := BuildStarItemShardPath(1), BuildStarItemShardPath(2), BuildStarItemShardPath(3)
	for name, c := range map[string]struct {
		branchReads   map[string]string
		wantBlobReads int32
	}{
		"all match":      {nil, 0},
		"another commit": {map[string]string{shard1: `[{"id":9,"name":"a/nine","note":"old"}]`}, 1},
		"passing 404":    {map[string]string{shard2: ""}, 1},
	} {
		repo, blobReads := startFakeShardAPI(t, files, c.branchReads)
		items, err := starItemsDataFile.readLatestFromDBRepo(ctx, repo)
		if ids := listTestStarItemIDs(items); err != nil || !slices.Equal(ids, []int64{9, 2}) || items[0].Note != "new" || blobReads.Load() != c.wantBlobReads {
			t.Errorf("%s: %v %+v, %v, blob reads %d", name, ids, items, err, blobReads.Load())
		}
	}

	withoutShard3 := newTestShardContents(map[int]string{3: ""})
	repo, _ := startFakeShardAPI(t, withoutShard3, map[string]string{shard3: "[]\n"})
	var httpErr *HTTPError
	if _, err := starItemsDataFile.readLatestFromDBRepo(ctx, repo); !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway || !strings.Contains(err.Error(), shard3) {
		t.Errorf("not listed: %v", err)
	}

	repo, blobReads := startFakeShardAPI(t, files, map[string]string{shard1: `[{"id":9,"name":"a/nine","note":"old"}]`})
	if items, err := starItemsDataFile.readFromDBRepoAtCommit(ctx, repo, "c1"); err != nil || len(items) != 2 || items[0].Note != "new" || blobReads.Load() != 0 {
		t.Errorf("at a commit: %+v, %v, blob reads %d", items, err, blobReads.Load())
	}
}

// Runs `f`, and returns what it panicked with. `nil` if it didn't panic.
// `recover` stops a panic inside a deferred function, like catching an exception in C++
func catchPanic(f func()) (recovered any) {
	defer func() { recovered = recover() }()
	f()
	return nil
}
