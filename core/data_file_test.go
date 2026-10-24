// Unit tests for the data files of the archive (`data_file.go`)
package core

import (
	"fmt"
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

// Runs `f`, and returns what it panicked with. `nil` if it didn't panic.
// `recover` stops a panic inside a deferred function, like catching an exception in C++
func catchPanic(f func()) (recovered any) {
	defer func() { recovered = recover() }()
	f()
	return nil
}
