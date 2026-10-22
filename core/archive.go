// The archive as a whole: the three archive files read and written together in one commit (`Archive`, `UpdateArchive`),
// and the data file format (`encodeLines`).
// Each part has its own file: entries in `archive_star_items.go`, star lists in `archive_star_lists.go`,
// metadata snapshots in `archive_repo_meta.go`, and README snapshots in `archive_readmes.go`.
// Writing goes through the storage engine (`UpdateDBRepoFiles` in `db_repo_io.go`).
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"time"
)

// Decodes the content of a data file into a list of items.
// `[T any]` is a type parameter, like a C++ template, so the same function works for each item type.
//
// Args:
//   - `content`: The file content. `nil` (no file) gives an empty list
func decodeItems[T any](content []byte) ([]T, error) {
	items := []T{}
	if content != nil {
		if err := json.Unmarshal(content, &items); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// Reads the data file `path` and decodes it into a list of items.
// Used when only reading, not writing
func readItems[T any](ctx context.Context, repo *DBRepo, path string) ([]T, error) {
	content, err := ReadDBRepoFile(ctx, repo, path)
	if err != nil {
		return nil, err
	}
	return decodeItems[T](content)
}

// All archive data.
// `UpdateArchive` reads the three files together from one commit and puts them here
type Archive struct {
	StarItems            []StarItem              // Archive entries (`StarItemsDBPath`)
	StarLists            []StarList              // Star list definitions (`StarListsDBPath`)
	RepoMeta             []RepoMeta              // Snapshots of repository metadata (`RepoMetaDBPath`)
	fetchedReadmesToSave map[int64]FetchedReadme // READMEs to save in the same commit (`AddFetchedReadmeToSave`): item ID -> README
}

// Adds `fetched` to the READMEs to save as the README snapshot of item `itemID`.
// They are written in the commit that `UpdateArchive` makes for the change, not when this is called.
// Call it on the archive given to the `change` of `UpdateArchive`.
// Called on any other archive, it saves nothing.
// The rules for what is written and deleted are in `planReadmeSnapshotUpdate`.
// It is ignored if the item isn't in the archive when the change ends.
func (a *Archive) AddFetchedReadmeToSave(itemID int64, fetched FetchedReadme) {
	if a.fetchedReadmesToSave == nil {
		a.fetchedReadmesToSave = map[int64]FetchedReadme{}
	}
	a.fetchedReadmesToSave[itemID] = fetched
}

// Reads the three archive files from one commit, changes them with `change`, and writes only the changed files in one commit.
// It works on top of `UpdateDBRepoFiles`.
//
// Args:
//   - `change`: Changes the given archive in place and returns a commit message.
//     If it returns an error, nothing is written.
//     If another commit gets in, it is called again with the new archive
//
// Returns:
//   - The archive after the change. If no file changed, nothing is committed and it is returned as is
//
// NOTE: The three files are read together from the same commit.
// So checks like "does this star list exist" and the save use data from the same point.
// Whether a file changed is decided by writing the data before and after `change` in the same format (`encodeLines`) and comparing.
// So a file with the same data in a different layout (for example, edited by hand) is not rewritten.
// A missing file counts as an empty list, so a file that nobody touched isn't created as an empty file.
// The config file (`config.json`) is not part of the archive, so it is not read here.
// This keeps each save request from making one extra read for nothing.
// The README snapshots of items that `change` removes are deleted in the same commit, so none is left without its item.
// READMEs given to `AddFetchedReadmeToSave` are written in the same commit too.
// Each removed item and each saved README costs one more request (a listing of its folder).
func UpdateArchive(ctx context.Context, repo *DBRepo, change func(a *Archive) (CommitMessage, error)) (*Archive, error) {
	// Turns the three data files into file content (the `encodeLines` format).
	// Returns path -> content
	encode := func(a *Archive) (map[string][]byte, error) {
		files := map[string][]byte{}
		var err error
		if files[StarItemsDBPath], err = encodeLines(a.StarItems); err != nil {
			return nil, err
		}
		if files[StarListsDBPath], err = encodeLines(a.StarLists); err != nil {
			return nil, err
		}
		if files[RepoMetaDBPath], err = encodeLines(a.RepoMeta); err != nil {
			return nil, err
		}
		return files, nil
	}
	var updated *Archive
	// Reads the three files together from the same commit
	err := UpdateDBRepoFiles(ctx, repo, []string{StarItemsDBPath, StarListsDBPath, RepoMetaDBPath}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
		a := &Archive{}
		var err error
		if a.StarItems, err = decodeItems[StarItem](filesRead[StarItemsDBPath]); err != nil {
			return FileChangesCommit{}, err
		}
		if a.StarLists, err = decodeItems[StarList](filesRead[StarListsDBPath]); err != nil {
			return FileChangesCommit{}, err
		}
		if a.RepoMeta, err = decodeItems[RepoMeta](filesRead[RepoMetaDBPath]); err != nil {
			return FileChangesCommit{}, err
		}
		before, err := encode(a) // `change` edits `a` in place, so encode it first
		if err != nil {
			return FileChangesCommit{}, err
		}
		idsBefore := make([]int64, 0, len(a.StarItems)) // IDs of the items before `change`, to find the ones it removes
		for _, item := range a.StarItems {
			idsBefore = append(idsBefore, item.ID)
		}

		message, err := change(a)
		if err != nil {
			return FileChangesCommit{}, err
		}

		after, err := encode(a)
		if err != nil {
			return FileChangesCommit{}, err
		}
		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}, Message: message}
		for path, content := range after {
			if !bytes.Equal(before[path], content) {
				changes.FilesToWrite[path] = content
			}
		}

		// Deletes the README snapshots of the items that `change` removed, in this same commit.
		// An item is removed if its ID is in `idsBefore` but no longer in `a.StarItems`
		isArchivedAfter := make(map[int64]bool, len(a.StarItems)) // Item ID -> `true`, for the items after `change`
		for _, item := range a.StarItems {
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

		// Saves the READMEs given to `AddFetchedReadmeToSave`, against the snapshots already saved for those items
		for id, fetched := range a.fetchedReadmesToSave {
			if !isArchivedAfter[id] {
				continue // A README of an item not in the archive would have no item to own it
			}
			snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, commitSHA, buildReadmeFolderPath(id))
			if err != nil {
				return FileChangesCommit{}, err
			}
			planReadmeSnapshotUpdate(id, fetched, snapshotsByID[id], &changes)
		}
		updated = a // If called again, this becomes the new archive
		return changes, nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Turns a list of items into the data file format.
// The format is a JSON array with one item per line (`[\n{...},\n{...}\n]\n`), or `[]\n` if empty.
// All data file content is made by this function.
//
// Args:
//   - `items`: The items. The key order is the field order of the item struct
//
// NOTE: With one item per line, the diff can be read line by line, and commits made at the same time can be resolved with a rebase.
func encodeLines[T any](items []T) ([]byte, error) {
	if len(items) == 0 {
		return []byte("[]\n"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, item := range items {
		if i > 0 {
			buf.WriteString(",\n")
		}
		var line bytes.Buffer
		enc := json.NewEncoder(&line)
		enc.SetEscapeHTML(false) // Keeps `<`, `>`, `&` as they are instead of Unicode escapes, so the file reads as plain text
		if err := enc.Encode(item); err != nil {
			return nil, err
		}
		buf.Write(bytes.TrimRight(line.Bytes(), "\n")) // Removes the newline that `Encode` adds, so the separator between items is set here
	}
	buf.WriteString("\n]\n")
	return buf.Bytes(), nil
}

// Formats the current time the way archive files store it (UTC, with milliseconds, e.g. `2026-01-01T00:00:00.000Z`)
func FormatNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
