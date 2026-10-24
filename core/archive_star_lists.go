// Star lists: the unit that groups archive items (`db/star-lists.json`).
// This file also holds name comparison, ID issuing, and cleanup of the IDs stored in items.
// Changing the star list definitions alone is done by `UpdateStarLists` here.
// Changes that also touch the items go through `UpdateStarCatalog`, and the rules for the star list IDs stored in items are there too (`archive_star_catalog.go`).
// Reading the star lists on GitHub is in `github_stars.go`.
package core

import (
	"context"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// One star list.
// An archive item can belong to several star lists.
// It is the same idea as GitHub's star lists.
// When code refers to a star list on GitHub, its name always has `GitHub` in it (`FetchGitHubStarListsByRepo`)
type StarList struct {
	ID          int64  `json:"id"`          // Creation time (milliseconds). The server issues it (`NewStarListID`)
	Name        string `json:"name"`        // Name shown on screen. The archive has only one star list per name (by `IsSameStarListName`)
	Description string `json:"description"` // Description. Empty string if there is none
}

// Reports whether star list names `a` and `b` point to the same star list.
// It ignores leading and trailing spaces, Unicode normalization (NFC) differences, and letter case.
//
// NOTE: "Is this the same star list?" is decided only by this function.
// Both the duplicate name check and the import of GitHub star lists use it.
// With two rules, a star list made in the dashboard and one imported from GitHub could exist side by side.
func IsSameStarListName(a string, b string) bool {
	return normalizeStarListName(a) == normalizeStarListName(b)
}

// Turns a star list name into a string for comparison.
// It trims leading and trailing spaces, normalizes to NFC, and converts to lower case.
//
// Args:
//   - `name`: Name typed by a person or received from GitHub
//
// NOTE: Korean typed on macOS can arrive with its letters split into parts (NFD).
// It looks the same but the bytes differ, so names are normalized to NFC before comparing.
func normalizeStarListName(name string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(name)))
}

// Picks the ID of a new star list: the larger of the current time (milliseconds) and "largest current ID + 1".
//
// Args:
//   - `starLists`: Star lists in the archive now. The new ID doesn't match any of them
//
// NOTE: With the time alone, the ID can clash with an existing one.
// That happens when two are made in the same millisecond, or when the server clock goes back.
// Making it larger than the largest ID prevents this.
func NewStarListID(starLists []StarList) int64 {
	id := time.Now().UnixMilli()
	for _, l := range starLists {
		id = max(id, l.ID+1)
	}
	return id
}

// Reads the star list definitions from one commit, changes them with `change`, and writes them in one commit if they changed.
// Only the star list file is read, so it is for changes that don't look at the items.
// It works on top of `UpdateDBRepoFiles`.
//
// Args:
//   - `change`: Changes `starLists` in place and returns a commit message.
//     It gets a pointer to the slice, so it can also append to it or replace it.
//     If it returns an error, nothing is written.
//     If another commit gets in, it is called again with the star lists read at the new commit
func UpdateStarLists(ctx context.Context, repo *DBRepo, change func(starLists *[]StarList) (CommitMessage, error)) error {
	return UpdateDBRepoFiles(ctx, repo, []string{starListsDataFile.path}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
		var starLists []StarList
		trackedStarLists, err := starListsDataFile.decodeForWriting(filesRead, &starLists)
		if err != nil {
			return FileChangesCommit{}, err
		}
		message, err := change(&starLists)
		if err != nil {
			return FileChangesCommit{}, err
		}
		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}, Message: message}
		if err := trackedStarLists.addIfChanged(&changes); err != nil {
			return FileChangesCommit{}, err
		}
		return changes, nil
	})
}
