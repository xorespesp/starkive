// The star catalog: the archive items (`db/star-items.json`, `StarItem`) and the star list definitions
// (`db/star-lists.json`, `StarList`), the user's own data.
// It holds both types, the rules about star lists (name comparison, ID issuing, the star list IDs stored in items),
// and the writes: `UpdateStarCatalog` changes both in one commit, `UpdateStarLists` changes the star lists alone,
// and `AddStarredReposToStarCatalog` puts GitHub Stars in as new items.
// The snapshots are in `archive_repo_meta.go` (metadata) and `archive_readmes.go` (README).
// Reading Stars and star lists from GitHub is in `github_stars.go`.
// The file format is in `data_file.go`, and writing goes through the storage engine (`UpdateDBRepoFiles` in `db_repo_io.go`).
package core

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"
)

// The star catalog: the archive items and the star list definitions.
// It is the user's own data.
// The snapshots (metadata, README) aren't in it, because sync updates them on its own.
type StarCatalog struct {
	StarItems []StarItem // Archive items (`StarItemsDBPath`)
	StarLists []StarList // Star list definitions (`StarListsDBPath`)
}

// One archive entry (one line of `db/star-items.json`).
// The field order is the key order in the file.
// Changing the order turns the whole file into a diff.
//
// NOTE: Values that the repository owner can change over time (description, language, Star count, etc.) are not stored here.
// Stored copies would drift from the real values.
// The page fetches such values fresh from `/api/github-repo-meta` each time, and the snapshot for the record lives in `RepoMeta`.
type StarItem struct {
	ID          int64   `json:"id"`                // GitHub repository ID. It stays the same when the name changes, so entries are identified by it
	NodeID      string  `json:"node_id,omitempty"` // ID for GraphQL queries (new format `R_kgDO...`)
	Name        string  `json:"name"`              // `owner/repo` at the time it was archived. Shown only when the repository is gone and can't be fetched
	Note        string  `json:"note"`              // The user's note
	StarListIDs []int64 `json:"star_list_ids"`     // IDs of the star lists it belongs to. Always cleaned by `StarCatalog.CleanStarListIDs`
	StarredAt   string  `json:"starred_at"`        // When it was starred on GitHub. For entries added by hand, when it was added
}

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

// Checks that all requested star list IDs exist in this catalog, and returns them cleaned up for storing in an item.
// `(c *StarCatalog)` marks this function as a method of `StarCatalog`, and `c` plays the role of `this` in C++.
//
// Args:
//   - `ids`: Star list IDs from the request body
//
// Returns:
//   - The cleaned ID list (same rules as `CleanStarListIDs`).
//     A 400 `HTTPError` if even one ID doesn't exist
//
// NOTE: Call it inside the `change` of `UpdateStarCatalog`.
// Then the "does this star list exist?" check and the save use the catalog at the same commit.
// An ID of a star list deleted in the meantime can't be saved.
func (c *StarCatalog) CheckStarListIDs(ids []int64) ([]int64, error) {
	for _, id := range ids {
		if !slices.ContainsFunc(c.StarLists, func(l StarList) bool { return l.ID == id }) {
			return nil, Errorf(http.StatusBadRequest, "Unknown star list id: %d", id)
		}
	}
	return c.CleanStarListIDs(ids), nil
}

// Cleans up the star list IDs to store in an item.
// It keeps only IDs that exist in this catalog, removes duplicates, and sorts by ID (which is creation order).
//
// Args:
//   - `ids`: IDs to clean up. IDs that don't exist are dropped without an error
//
// NOTE: Every place that writes star list IDs into an item goes through this method.
// So there is one ordering rule for the stored file.
// The same content always gives the same line, so no diff appears.
func (c *StarCatalog) CleanStarListIDs(ids []int64) []int64 {
	clean := []int64{}
	for _, id := range ids {
		exists := slices.ContainsFunc(c.StarLists, func(l StarList) bool { return l.ID == id })
		if exists && !slices.Contains(clean, id) {
			clean = append(clean, id)
		}
	}
	slices.Sort(clean)
	return clean
}

// Turns star list names into IDs.
// If no star list has the same name (by `IsSameStarListName`), it makes a new one in `c.StarLists`.
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
func (c *StarCatalog) getOrCreateStarListIDs(names []string) (map[string]int64, []string) {
	ids := map[string]int64{}
	created := []string{}
	for _, name := range names {
		i := slices.IndexFunc(c.StarLists, func(l StarList) bool { return IsSameStarListName(l.Name, name) })
		if i < 0 {
			c.StarLists = append(c.StarLists, StarList{ID: NewStarListID(c.StarLists), Name: strings.TrimSpace(name)})
			i = len(c.StarLists) - 1
			created = append(created, c.StarLists[i].Name)
		}
		ids[name] = c.StarLists[i].ID
	}
	return ids, created
}

// One change of the star catalog, made as one commit (`UpdateStarCatalog`).
// It holds a working copy of the catalog read at that commit, and the other files to write with it.
type StarCatalogTransaction struct {
	// Working copy. What it holds when `change` returns is committed.
	// It is embedded (no field name), so its fields and methods are used directly, like `tx.StarItems`.
	StarCatalog
	readmesToSave map[int64]FetchedReadme // Not catalog data: READMEs to save in the same commit (`SaveReadme`). Item ID -> README
}

// Saves `fetched` as the README snapshot of item `itemID`, in the commit that this transaction makes.
// It is written when the transaction commits, not when this is called.
// It is dropped if the item isn't in the working copy when `change` returns.
// The rules for what is written and deleted are in `planReadmeSnapshotUpdate`.
func (tx *StarCatalogTransaction) SaveReadme(itemID int64, fetched FetchedReadme) {
	if tx.readmesToSave == nil {
		tx.readmesToSave = map[int64]FetchedReadme{}
	}
	tx.readmesToSave[itemID] = fetched
}

// Reads the star catalog from one commit, changes it with `change`, and writes only the changed files in one commit.
// It works on top of `UpdateDBRepoFiles`.
//
// Args:
//   - `change`: Changes the working copy in `tx` in place and returns a commit message.
//     It can ask for READMEs to be saved in the same commit (`SaveReadme`).
//     If it returns an error, nothing is written.
//     If another commit gets in, it is called again with a new transaction read at the new commit
//
// Returns:
//   - The catalog as committed. If no file changed, nothing is committed and it is returned as is
//
// NOTE: The items and the star lists are read together from the same commit.
// So checks like "does this star list exist" and the save use data from the same point.
// The metadata snapshots aren't read.
// They are the largest archive file, and no change of the catalog uses them (measured, 20,000 items: 6MB of 9.2MB).
// Only the files whose data changed are written (`trackedDataFile`).
// The config file (`config.json`) is not part of the archive, so it is not read here.
// This keeps each save request from making one extra read for nothing.
// The README snapshots of items that `change` removes are deleted in the same commit, so none is left without its item.
// Each removed item and each saved README costs one more request (a listing of its folder).
func UpdateStarCatalog(ctx context.Context, repo *DBRepo, change func(tx *StarCatalogTransaction) (CommitMessage, error)) (*StarCatalog, error) {
	var committed *StarCatalog
	err := UpdateDBRepoFiles(ctx, repo, []string{starItemsDataFile.path, starListsDataFile.path}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
		tx := &StarCatalogTransaction{}
		trackedStarItems, err := starItemsDataFile.decodeForWriting(filesRead, &tx.StarItems)
		if err != nil {
			return FileChangesCommit{}, err
		}
		trackedStarLists, err := starListsDataFile.decodeForWriting(filesRead, &tx.StarLists)
		if err != nil {
			return FileChangesCommit{}, err
		}
		idsBefore := make([]int64, 0, len(tx.StarItems)) // IDs of the items before `change`, to find the ones it removes
		for _, item := range tx.StarItems {
			idsBefore = append(idsBefore, item.ID)
		}

		message, err := change(tx)
		if err != nil {
			return FileChangesCommit{}, err
		}

		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}, Message: message}
		if err := trackedStarItems.addIfChanged(&changes); err != nil {
			return FileChangesCommit{}, err
		}
		if err := trackedStarLists.addIfChanged(&changes); err != nil {
			return FileChangesCommit{}, err
		}

		// Deletes the README snapshots of the items that `change` removed, in this same commit.
		// An item is removed if its ID is in `idsBefore` but no longer in `tx.StarItems`
		isArchivedAfter := make(map[int64]bool, len(tx.StarItems)) // Item ID -> `true`, for the items after `change`
		for _, item := range tx.StarItems {
			isArchivedAfter[item.ID] = true
		}
		removedIDs := []int64{}
		for _, id := range idsBefore {
			if !isArchivedAfter[id] && !slices.Contains(removedIDs, id) { // An ID listed twice in the archive is removed once
				removedIDs = append(removedIDs, id)
			}
		}
		for _, id := range removedIDs {
			readmePaths, err := readReadmeSnapshotPaths(ctx, repo, commitSHA, id)
			if err != nil {
				return FileChangesCommit{}, err
			}
			changes.FilesToDelete = append(changes.FilesToDelete, readmePaths...)
		}

		// Saves the READMEs given to `SaveReadme`, against the snapshots already saved for those items
		for id, fetched := range tx.readmesToSave {
			if !isArchivedAfter[id] {
				continue // A README of an item not in the archive would have no item to own it
			}
			snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, commitSHA, buildReadmeFolderPath(id))
			if err != nil {
				return FileChangesCommit{}, err
			}
			planReadmeSnapshotUpdate(id, fetched, snapshotsByID[id], &changes)
		}
		committed = &tx.StarCatalog // If called again, this becomes the new catalog
		return changes, nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
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

// Puts GitHub Stars at the front of the archive as new items.
// Every path that puts Stars into the archive (sync, Import) goes through this function.
//   - Skips repositories already in the archive (by ID), and repositories repeated within `starredRepos`.
//     Existing items are left as they are
//   - Puts new items in the star lists of `starListIDs` and in the star lists they belonged to on GitHub.
//     If the archive has no star list with the same name, one is made
//   - New star lists and new items are written together in one commit.
//     The commit subject is `<commitPrefix>: archive N stars`.
//     The body lists the added repositories and the names of new star lists
//
// Args:
//   - `starredRepos`: Candidates to add, added in order (in GitHub's order, the most recently starred come first)
//   - `starListIDs`: Star list IDs that all new items go into (chosen by a person). An empty list if none.
//     A 400 `HTTPError` if an ID isn't in the archive
//   - `commitPrefix`: First part of the commit subject, `<who>(star-items)` (e.g. `sync(star-items)`)
//
// Returns:
//   - Number of items actually added
//
// NOTE: The duplicate check and the star list check are both done inside the `change` of `UpdateStarCatalog`.
// That is, they use the catalog right before the commit.
// So sending the same request again gives the same result.
// Star lists are made only when there are items to add.
// No item points to a star list that doesn't exist.
func AddStarredReposToStarCatalog(ctx context.Context, repo *DBRepo, starredRepos []StarredRepo, starListIDs []int64, commitPrefix string) (int, error) {
	// GitHub star lists are read only when there are new repositories to add (most daily syncs have none).
	// They aren't read again when `change` is called again after a conflict.
	// They are input from GitHub, not the archive, so another commit doesn't make them stale
	var githubStarLists map[int64][]string
	added := 0
	_, err := UpdateStarCatalog(ctx, repo, func(tx *StarCatalogTransaction) (CommitMessage, error) {
		added = 0 // If called again after a conflict, count again from zero
		// Both the star list check and the duplicate check below use the catalog right before the commit (`tx`)
		requested, err := tx.CheckStarListIDs(starListIDs)
		if err != nil {
			return CommitMessage{}, err
		}
		seen := make(map[int64]bool, len(tx.StarItems)+len(starredRepos))
		for _, s := range tx.StarItems {
			seen[s.ID] = true
		}
		if !slices.ContainsFunc(starredRepos, func(s StarredRepo) bool { return !seen[s.Repo.ID] }) {
			return CommitMessage{}, nil // Nothing to add. No file changed, so no commit is made
		}
		if githubStarLists == nil {
			if githubStarLists, err = FetchGitHubStarListsByRepo(ctx, repo.cfg); err != nil {
				return CommitMessage{}, err
			}
		}
		fresh := []StarItem{}
		createdStarLists := []string{}
		for _, s := range starredRepos {
			if seen[s.Repo.ID] {
				continue
			}
			seen[s.Repo.ID] = true // Even if the same repository appears again in `starredRepos`, add it only once
			idByName, created := tx.getOrCreateStarListIDs(githubStarLists[s.Repo.ID])
			createdStarLists = append(createdStarLists, created...)
			ids := append([]int64{}, requested...) // Appends to a copy. Appending to `requested` directly could overwrite its array when it has spare capacity
			for _, id := range idByName {
				ids = append(ids, id)
			}
			fresh = append(fresh, StarItem{
				ID: s.Repo.ID, NodeID: s.Repo.NodeID, Name: s.Repo.FullName, StarListIDs: tx.CleanStarListIDs(ids),
				StarredAt: s.StarredAt,
			})
		}
		added = len(fresh)
		names := make([]string, len(fresh))
		for i, s := range fresh {
			names[i] = s.Name
		}
		tx.StarItems = append(fresh, tx.StarItems...)
		return NewCommitMessage(commitPrefix, "archive "+FormatCount(added, "star"),
			formatCommitList("", names), formatCommitList("New star lists", createdStarLists)), nil
	})
	return added, err
}
