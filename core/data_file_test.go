// Unit tests for the data files of the archive (`data_file.go`)
package core

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// Tests that asking for a file not read at the commit panics, instead of looking like a file that doesn't exist.
// A file that was read but doesn't exist (`nil`) is an empty list, and `DBRepoFilesAtCommit.HasFile` tells it apart.
// A file that holds `null` is an empty list too
func TestDataFileRefusesFileNotRead(t *testing.T) {
	notRead := &DBRepoFilesAtCommit{commitSHA: "c1", contents: map[string][]byte{StarListsDBPath: []byte("[]\n")}}
	var starItems []StarItem
	for name, ask := range map[string]func(){
		"decodeForReading": func() { starItemsDataFile.decodeForReading(notRead) },
		"decodeForWriting": func() { starItemsDataFile.decodeForWriting(notRead, &starItems) },
		"HasFile":          func() { notRead.HasFile(StarItemsDBPath) },
		"empty value":      func() { (&DBRepoFilesAtCommit{}).FileContent(StarItemsDBPath) },
	} {
		if recovered := catchPanic(ask); recovered == nil || !strings.Contains(fmt.Sprint(recovered), StarItemsDBPath) {
			t.Errorf("%s, file not read: panic %v", name, recovered)
		}
	}
	items, err := starItemsDataFile.decodeForReading(&DBRepoFilesAtCommit{contents: map[string][]byte{StarItemsDBPath: nil}})
	if err != nil || items == nil || len(items) != 0 || (&DBRepoFilesAtCommit{contents: map[string][]byte{StarItemsDBPath: nil}}).HasFile(StarItemsDBPath) {
		t.Errorf("missing file: %v, %v", items, err)
	}
	items, err = starItemsDataFile.decodeForReading(&DBRepoFilesAtCommit{contents: map[string][]byte{StarItemsDBPath: []byte("null")}})
	if err != nil || items == nil || len(items) != 0 || !(&DBRepoFilesAtCommit{contents: map[string][]byte{StarItemsDBPath: []byte("null")}}).HasFile(StarItemsDBPath) {
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

// Makes the files read at one commit with every shard: shard index -> content.
// A shard not in `contentByIndex` holds `[]`, and an empty string is a shard that doesn't exist there
func newTestShardFiles(contentByIndex map[int]string) *DBRepoFilesAtCommit {
	files := &DBRepoFilesAtCommit{commitSHA: "c1", contents: map[string][]byte{}}
	for index := range StarItemShardCount {
		content, ok := contentByIndex[index]
		switch {
		case !ok:
			files.contents[BuildStarItemShardPath(index)] = []byte("[]\n")
		case content == "":
			files.contents[BuildStarItemShardPath(index)] = nil
		default:
			files.contents[BuildStarItemShardPath(index)] = []byte(content)
		}
	}
	return files
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
	if paths := starItemsShardedDataFile.buildPaths(); len(paths) != StarItemShardCount || paths[0] != "db/star-items/00.json" || paths[7] != "db/star-items/07.json" {
		t.Errorf("all shard paths: %v", paths)
	}
}

// Tests that `shardedStarItemsDataFile` refuses, when decoding for reading and for writing, a shard as a whole when it breaks the layout:
// an ID that isn't positive, an item in another shard, or an ID listed twice.
// A missing shard is a 502, not an empty shard.
// Good shards are joined in index order, each in file order
func TestShardedStarItemsDataFileRefusesBrokenShards(t *testing.T) {
	decoders := map[string]func(files *DBRepoFilesAtCommit) ([]StarItem, error){
		"for reading": starItemsShardedDataFile.decodeForReading,
		"for writing": func(files *DBRepoFilesAtCommit) ([]StarItem, error) {
			var items []StarItem
			_, err := starItemsShardedDataFile.decodeForWriting(files, &items)
			return items, err
		},
	}
	for decoderName, decode := range decoders {
		for name, content := range map[string]string{
			"zero ID":      `[{"id":0,"name":"a/zero"}]`,
			"negative ID":  `[{"id":-7,"name":"a/neg"}]`,
			"wrong shard":  `[{"id":2,"name":"a/two"}]`,
			"listed twice": `[{"id":9,"name":"a/nine"},{"id":9,"name":"a/nine"}]`,
			"not an array": `{"id":1}`,
		} {
			if items, err := decode(newTestShardFiles(map[int]string{1: content})); err == nil || items != nil || !strings.Contains(err.Error(), "db/star-items/01.json") {
				t.Errorf("%s, %s: %v, %v", decoderName, name, items, err)
			}
		}

		var httpErr *HTTPError
		if _, err := decode(newTestShardFiles(map[int]string{3: ""})); !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway || !strings.Contains(err.Error(), "db/star-items/03.json") {
			t.Errorf("%s, missing shard: %v", decoderName, err)
		}

		items, err := decode(newTestShardFiles(map[int]string{
			0: `[{"id":8,"name":"a/eight"}]`,
			1: `[{"id":9,"name":"a/nine"},{"id":1,"name":"a/one"}]`,
		}))
		if ids := listTestStarItemIDs(items); err != nil || !slices.Equal(ids, []int64{8, 9, 1}) {
			t.Errorf("%s, good shards: %v, %v", decoderName, ids, err)
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
	tracked, err := starItemsShardedDataFile.decodeForWriting(newTestShardFiles(map[int]string{
		1: "[\n  {\"id\":1,\"node_id\":\"\",\"name\":\"a/one\",\"note\":\"\",\"star_list_ids\":[],\"starred_at\":\"\"}\n]\n", // Another layout
		2: "[\n{\"id\":2,\"name\":\"b/two\",\"note\":\"\",\"star_list_ids\":[],\"starred_at\":\"\"}\n]\n",
	}), &items)
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

// Tests that a change which edits the star list IDs of an item in place is still written.
// The items share their `StarListIDs` arrays with the shards as decoded, so each shard as read must be encoded before the change
func TestTrackedShardedStarItemsDataFileSeesAChangeMadeInPlace(t *testing.T) {
	var items []StarItem
	tracked, err := starItemsShardedDataFile.decodeForWriting(newTestShardFiles(map[int]string{
		1: "[\n{\"id\":1,\"node_id\":\"\",\"name\":\"a/one\",\"note\":\"\",\"star_list_ids\":[100,200],\"starred_at\":\"\"}\n]\n",
	}), &items)
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

// Runs `f`, and returns what it panicked with. `nil` if it didn't panic.
// `recover` stops a panic inside a deferred function, like catching an exception in C++
func catchPanic(f func()) (recovered any) {
	defer func() { recovered = recover() }()
	f()
	return nil
}
