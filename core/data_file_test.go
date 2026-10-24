// Unit tests for the data files of the archive (`data_file.go`)
package core

import (
	"strings"
	"testing"
)

// Tests that a file not given to `UpdateDBRepoFiles` is an error, not an empty file.
// A file that was read but doesn't exist (`nil`) is an empty list
func TestDataFileDecodeRefusesFileNotRead(t *testing.T) {
	if _, err := starItemsDataFile.decodeForReading(map[string][]byte{StarListsDBPath: []byte("[]\n")}); err == nil || !strings.Contains(err.Error(), StarItemsDBPath) {
		t.Errorf("file not read: %v", err)
	}
	var starItems []StarItem
	if _, err := starItemsDataFile.decodeForWriting(map[string][]byte{}, &starItems); err == nil {
		t.Error("file not read, decodeForWriting: no error")
	}
	items, err := starItemsDataFile.decodeForReading(map[string][]byte{StarItemsDBPath: nil})
	if err != nil || items == nil || len(items) != 0 {
		t.Errorf("missing file: %v, %v", items, err)
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
		tracked, err := starListsDataFile.decodeForWriting(map[string][]byte{StarListsDBPath: c.content}, &starLists)
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
