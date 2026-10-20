// Unit tests for the README snapshot files (`readme.go`)
package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Tests how the README file content is made and read back.
//   - Short text is kept as it is. A UTF-8 byte order mark is removed, and UTF-16 with a byte order mark is turned into UTF-8
//   - Blank text, text with a NUL byte, and text that isn't UTF-8 aren't usable
//   - Long text is cut after its last line break within `maxReadmeBytes`, or at a character boundary if it has none,
//     and the cut line is added
//   - Text of exactly `maxReadmeBytes` isn't cut
func TestBuildReadmeFile(t *testing.T) {
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
		{"NUL byte", "a\x00b", ""},
		{"not UTF-8", "a\xC3(", ""},
		{"exactly the limit", strings.Repeat("a", maxReadmeBytes), strings.Repeat("a", maxReadmeBytes)},
		{"cut at a line break", strings.Repeat(strings.Repeat("x", 99)+"\n", 100),
			strings.Repeat(strings.Repeat("x", 99)+"\n", 81) + readmeCutLine + "\n"},
		{"cut at a character boundary", strings.Repeat("가", 3000), strings.Repeat("가", 2730) + "\n" + readmeCutLine + "\n"},
	}
	for _, c := range cases {
		got, usable := buildReadmeFile([]byte(c.sourceBytes))
		if usable != (c.want != "") || string(got) != c.want {
			t.Errorf("%s: got %q (usable %v)", c.name, got, usable)
		}
	}

	// Reading back gives the text without the cut line, and whether it was cut
	content, _ := buildReadmeFile([]byte(strings.Repeat(strings.Repeat("x", 99)+"\n", 100)))
	if text, isCut := parseReadmeFile(content); !isCut || text != strings.Repeat(strings.Repeat("x", 99)+"\n", 81) {
		t.Errorf("parse cut file: cut %v, %d bytes", isCut, len(text))
	}
	if text, isCut := parseReadmeFile([]byte("# Title\n")); isCut || text != "# Title\n" {
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
