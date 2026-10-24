// The star catalog (`StarCatalog`): the archive items and the star list definitions, the user's own data.
// It holds the catalog's rules that look at both (the star list IDs stored in items),
// and the write that changes both in one commit (`StarCatalogTransaction`, `UpdateStarCatalog`).
// The items themselves are in `archive_star_items.go`, the star lists in `archive_star_lists.go`,
// the metadata snapshots in `archive_repo_meta.go`, and the README snapshots in `archive_readmes.go`.
// The file format is in `data_file.go`, and writing goes through the storage engine (`UpdateDBRepoFiles` in `db_repo_io.go`).
package core

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// The star catalog: the archive items and the star list definitions.
// It is the user's own data.
// The snapshots (metadata, README) aren't in it, because sync updates them on its own.
type StarCatalog struct {
	StarItems []StarItem // Archive items (`StarItemsDBPath`)
	StarLists []StarList // Star list definitions (`StarListsDBPath`)
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
