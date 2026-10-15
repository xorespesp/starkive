// Star lists: the unit that groups archive items (`db/star-lists.json`).
// This file also holds name comparison, ID issuing, and cleanup of the IDs stored in items.
// Reading and writing star list definitions is done by `UpdateArchive` (`store.go`).
// This file holds only the rules used inside it.
package core

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// One star list. An archive item can belong to several star lists.
// It is the same idea as GitHub's star lists.
// When code refers to a star list on GitHub, its name always has `GitHub` in it (`FetchGitHubStarListsByRepo`)
type StarList struct {
	ID          int64  `json:"id"`          // Creation time (milliseconds). The server issues it (`NewStarListID`)
	Name        string `json:"name"`        // Name shown on screen. The archive has only one star list per name (by `IsSameStarListName`)
	Description string `json:"description"` // Description. Empty string if there is none
}

// Reports whether two star list names point to the same star list.
// It ignores leading and trailing spaces, Unicode normalization (NFC) differences, and letter case.
//
// Args:
//   - `a`, `b`: The two names to compare
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

// Checks that all requested star list IDs exist in this archive, and returns them cleaned up for storing in an item.
// `(a *Archive)` marks this function as a method of `Archive`, and `a` plays the role of `this` in C++.
//
// Args:
//   - `ids`: Star list IDs from the request body
//
// Returns:
//   - The cleaned ID list (same rules as `CleanStarListIDs`).
//     A 400 `HTTPError` if even one ID doesn't exist
//
// NOTE: It is called inside the `change` of `UpdateArchive`.
// So the "does this star list exist?" check and the save use the archive at the same commit.
// An ID of a star list deleted in the meantime can't be saved.
func (a *Archive) CheckStarListIDs(ids []int64) ([]int64, error) {
	for _, id := range ids {
		if !slices.ContainsFunc(a.StarLists, func(l StarList) bool { return l.ID == id }) {
			return nil, Errorf(http.StatusBadRequest, "Unknown star list id: %d", id)
		}
	}
	return a.CleanStarListIDs(ids), nil
}

// Cleans up the star list IDs to store in an item.
// It keeps only IDs that exist in this archive, removes duplicates, and sorts by ID (which is creation order).
//
// Args:
//   - `ids`: IDs to clean up. IDs that don't exist are dropped without an error
//
// NOTE: Every place that writes star list IDs into an item goes through this method.
// So there is one ordering rule for the stored file.
// The same content always gives the same line, so no diff appears.
func (a *Archive) CleanStarListIDs(ids []int64) []int64 {
	clean := []int64{}
	for _, id := range ids {
		exists := slices.ContainsFunc(a.StarLists, func(l StarList) bool { return l.ID == id })
		if exists && !slices.Contains(clean, id) {
			clean = append(clean, id)
		}
	}
	slices.Sort(clean)
	return clean
}

// Turns star list names into IDs.
// If no star list has the same name (by `IsSameStarListName`), it makes a new one in `a.StarLists`.
//
// Args:
//   - `names`: GitHub star list names
//
// Returns:
//   - Given name -> star list ID
//   - Names of star lists made this time (written in the commit message body)
//
// NOTE: Names are turned into IDs only in this method, and after that only IDs are used.
// Names can change, so using them in place of IDs would drift.
func (a *Archive) getOrCreateStarListIDs(names []string) (map[string]int64, []string) {
	ids := map[string]int64{}
	created := []string{}
	for _, name := range names {
		i := slices.IndexFunc(a.StarLists, func(l StarList) bool { return IsSameStarListName(l.Name, name) })
		if i < 0 {
			a.StarLists = append(a.StarLists, StarList{ID: NewStarListID(a.StarLists), Name: strings.TrimSpace(name)})
			i = len(a.StarLists) - 1
			created = append(created, a.StarLists[i].Name)
		}
		ids[name] = a.StarLists[i].ID
	}
	return ids, created
}
