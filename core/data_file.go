// Data files of the archive: the files that hold one item per line as a JSON array
// (`db/star-items.json`, `db/star-lists.json`, `db/repo-meta.json`, and the item shards under `StarItemsDBDir`).
// This file holds their format (`encodeJSONArrayByLine`, `FormatNow`), which path holds which item type (`dataFile`),
// and how a write tells whether a file changed (`trackedDataFile`).
// The archive items are split by ID into shards, each a data file of its own.
// A write reads them as one list and writes back only the shards that changed (`shardedStarItemsDataFile`).
// What the items mean is in `archive_stars.go` (items and star lists) and `archive_repo_meta.go`.
// README snapshots aren't data files. They are in `archive_readmes.go`.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// One data file of the archive: its path, and the Go type of its items (`T`).
// `[T any]` is a type parameter, like a C++ template.
// `T` isn't stored. It only fixes the item type that the methods decode.
// Reach a data file only through the values below, and a shard of the archive items only through `starItemsShardedDataFile`.
// Then each path is always read as the type of its items.
type dataFile[T any] struct {
	path string // Path in the DB repository
}

// The data files of the archive
var (
	starItemsDataFile = dataFile[StarItem]{path: StarItemsDBPath}
	starListsDataFile = dataFile[StarList]{path: StarListsDBPath}
	repoMetaDataFile  = dataFile[RepoMeta]{path: RepoMetaDBPath}
)

// Reads this file at the latest commit of the default branch, and decodes it.
// It is for reading only.
// A write reads its files through `UpdateDBRepoFiles`, so they all come from the commit it builds on.
func (f dataFile[T]) readLatestFromDBRepo(ctx context.Context, repo *DBRepo) ([]T, error) {
	content, err := ReadDBRepoFile(ctx, repo, f.path)
	if err != nil {
		return nil, err
	}
	return decodeJSONArray[T](content)
}

// Decodes this file from `filesAtCommit`, the files that `UpdateDBRepoFiles` read at one commit.
// It is for a file that a write only reads: changes to the result aren't written.
// A file that a write may change is decoded with `decodeForWriting`.
//
// Returns:
//   - The items. An empty list if the file doesn't exist, the same as for a file that holds `[]`.
//     `DBRepoFilesAtCommit.HasFile` tells them apart.
//     It panics if this file wasn't among the paths given to `UpdateDBRepoFiles` (`DBRepoFilesAtCommit.FileContent`)
func (f dataFile[T]) decodeForReading(filesAtCommit *DBRepoFilesAtCommit) ([]T, error) {
	return decodeJSONArray[T](filesAtCommit.FileContent(f.path))
}

// Decodes this file from `filesAtCommit` into `*items`, and starts tracking that variable.
// After the change, `addIfChanged` on the result adds the file to the commit if `*items` changed.
//
// Args:
//   - `filesAtCommit`: The files that `UpdateDBRepoFiles` read at one commit
//   - `items`: The variable to decode into. The write must change this variable, not a copy of it
//
// Returns:
//   - An error if the file can't be decoded.
//     It panics if the file wasn't among the paths given to `UpdateDBRepoFiles` (`DBRepoFilesAtCommit.FileContent`)
func (f dataFile[T]) decodeForWriting(filesAtCommit *DBRepoFilesAtCommit, items *[]T) (*trackedDataFile[T], error) {
	decoded, err := f.decodeForReading(filesAtCommit)
	if err != nil {
		return nil, err
	}
	encodedAsRead, err := encodeJSONArrayByLine(decoded) // Kept before the change edits the items in place
	if err != nil {
		return nil, err
	}
	*items = decoded
	return &trackedDataFile[T]{path: f.path, encodedAsRead: encodedAsRead, items: items}, nil
}

// A data file decoded into a variable for a change (`decodeForWriting`).
// It keeps the file as read, to tell after the change whether the variable changed.
type trackedDataFile[T any] struct {
	path          string // Path in the DB repository
	encodedAsRead []byte // The items as read, in the standard format (`encodeJSONArrayByLine`)
	items         *[]T   // The variable the file was decoded into. A pointer, so the latest value is seen even if the change replaces the slice
}

// Adds the file to `changes` if the items in the tracked variable now differ from the items as read.
// `changes.FilesToWrite` must not be `nil` (writing to a `nil` map panics in Go).
//
// NOTE: Both sides are compared in the standard format (`encodeJSONArrayByLine`), not against the bytes as read.
// So a file with the same data in a different layout (for example, edited by hand) is not rewritten.
// A missing file is read as an empty list, so a file that nobody touched isn't created as an empty file.
func (t *trackedDataFile[T]) addIfChanged(changes *FileChangesCommit) error {
	encoded, err := encodeJSONArrayByLine(*t.items)
	if err != nil {
		return err
	}
	if !bytes.Equal(t.encodedAsRead, encoded) {
		changes.FilesToWrite[t.path] = encoded
	}
	return nil
}

// The archive items as one data file, split by ID into `StarItemShardCount` shards under `StarItemsDBDir`.
// Each shard is a data file of its own, holding only the items of that shard (`computeShardIndexOfStarItem`).
// It has the same methods as `dataFile`, so a write reads all shards as one list and writes back only the shards that changed.
// The item type is fixed to `StarItem`, because splitting needs the item ID.
type shardedStarItemsDataFile struct {
	shards [StarItemShardCount]dataFile[StarItem] // Data file of each shard, by shard index
}

// The archive items split into shards. Reach the shards only through this value
var starItemsShardedDataFile = newShardedStarItemsDataFile()

// Builds the data file of the archive items, with one data file per shard (`BuildStarItemShardPath`).
func newShardedStarItemsDataFile() shardedStarItemsDataFile {
	var f shardedStarItemsDataFile
	for index := range StarItemShardCount {
		f.shards[index] = dataFile[StarItem]{path: BuildStarItemShardPath(index)}
	}
	return f
}

// Builds the paths of all shards, to read them together with other files at one commit (`UpdateDBRepoFiles`).
func (f shardedStarItemsDataFile) buildPaths() []string {
	paths := make([]string, 0, StarItemShardCount)
	for _, shard := range f.shards {
		paths = append(paths, shard.path)
	}
	return paths
}

// Decodes all shards from `filesAtCommit`, the files that `UpdateDBRepoFiles` read at one commit, into one list.
// It is for a write that only reads the items: changes to the result aren't written.
// A write that may change the items uses `decodeForWriting`.
//
// Returns:
//   - The items, shard by shard in index order, and in file order within a shard.
//     An error if a shard can't be decoded, or is missing or breaks the layout (`checkStarItemShard`).
//     It panics if a shard wasn't among the paths given to `UpdateDBRepoFiles` (`DBRepoFilesAtCommit.FileContent`)
func (f shardedStarItemsDataFile) decodeForReading(filesAtCommit *DBRepoFilesAtCommit) ([]StarItem, error) {
	items := []StarItem{}
	for index, shard := range f.shards {
		shardItems, err := shard.decodeForReading(filesAtCommit)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shard.path, err)
		}
		if err := checkStarItemShard(filesAtCommit, index, shardItems); err != nil {
			return nil, err
		}
		items = append(items, shardItems...)
	}
	return items, nil
}

// Decodes all shards from `filesAtCommit` into `*items` as one list, and starts tracking that variable.
// After the change, `addIfChanged` on the result adds to the commit only the shards whose items changed.
//
// Args:
//   - `filesAtCommit`: The files that `UpdateDBRepoFiles` read at one commit
//   - `items`: The variable to decode into. The write must change this variable, not a copy of it
//
// Returns:
//   - An error if a shard can't be decoded, or is missing or breaks the layout (`checkStarItemShard`).
//     `*items` is set only when every shard is good.
//     It panics if a shard wasn't among the paths given to `UpdateDBRepoFiles` (`DBRepoFilesAtCommit.FileContent`)
func (f shardedStarItemsDataFile) decodeForWriting(filesAtCommit *DBRepoFilesAtCommit, items *[]StarItem) (*trackedShardedStarItemsDataFile, error) {
	tracked := &trackedShardedStarItemsDataFile{items: items}
	decoded := []StarItem{}
	for index, shard := range f.shards {
		var err error
		tracked.shards[index], err = shard.decodeForWriting(filesAtCommit, &tracked.shardItems[index])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shard.path, err)
		}
		if err := checkStarItemShard(filesAtCommit, index, tracked.shardItems[index]); err != nil {
			return nil, err
		}
		decoded = append(decoded, tracked.shardItems[index]...)
	}
	*items = decoded
	return tracked, nil
}

// The archive items decoded from all shards into one variable for a change (`shardedStarItemsDataFile.decodeForWriting`).
// Each shard is tracked as its own data file (`trackedDataFile`),
// and `addIfChanged` splits the one variable back into the shards before they are compared.
type trackedShardedStarItemsDataFile struct {
	shardItems [StarItemShardCount][]StarItem                 // Items of each shard. `shards` tracks these variables
	shards     [StarItemShardCount]*trackedDataFile[StarItem] // Each shard as read, to tell whether its items changed
	items      *[]StarItem                                    // The variable the shards were decoded into. A pointer, so the latest value is seen even if the change replaces the slice
}

// Splits the items in the tracked variable into shards by ID,
// and adds to `changes` only the shards whose items now differ from the items as read (`trackedDataFile.addIfChanged`).
// Within each shard, the items keep their order in the variable.
// `changes.FilesToWrite` must not be `nil` (writing to a `nil` map panics in Go).
//
// Returns:
//   - An error, with nothing added, if an ID isn't positive or appears twice.
//     The next read would refuse such a shard (`checkStarItemShard`), so it is never written.
//     An error if a shard can't be encoded
func (t *trackedShardedStarItemsDataFile) addIfChanged(changes *FileChangesCommit) error {
	var split [StarItemShardCount][]StarItem      // Items of each shard after the change
	isSeen := make(map[int64]bool, len(*t.items)) // Item ID -> `true` once seen
	for _, item := range *t.items {
		if item.ID <= 0 || isSeen[item.ID] {
			return fmt.Errorf("refusing to write star item %d: its ID isn't positive or appears twice", item.ID)
		}
		isSeen[item.ID] = true
		index := computeShardIndexOfStarItem(item.ID)
		split[index] = append(split[index], item)
	}
	t.shardItems = split // The tracked variables of `shards` now hold the items after the change
	for index := range StarItemShardCount {
		if err := t.shards[index].addIfChanged(changes); err != nil {
			return err
		}
	}
	return nil
}

// Checks shard `shardIndex`, whose items `items` were decoded from `filesAtCommit`.
// The shard must exist, and every item must have a positive ID that belongs to this shard (`computeShardIndexOfStarItem`).
// No ID may appear twice.
//
// Returns:
//   - A 502 (`newMissingStarItemShardError`) if the shard doesn't exist at the commit.
//     A data file that doesn't exist decodes as an empty list (`dataFile.decodeForReading`), so this keeps a missing shard from reading as an empty one.
//     An error if an item breaks the layout
//
// NOTE: An item in the wrong shard would show in the list, but a write would look for it only in its own shard.
// It could then never be changed or deleted, and Add would put a second copy in the right shard.
// So a shard that breaks the rule is refused as a whole, and a person fixes the file.
func checkStarItemShard(filesAtCommit *DBRepoFilesAtCommit, shardIndex int, items []StarItem) error {
	path := BuildStarItemShardPath(shardIndex)
	if !filesAtCommit.HasFile(path) {
		return newMissingStarItemShardError(path)
	}
	isSeen := make(map[int64]bool, len(items)) // Item ID -> `true` once seen in this shard
	for _, item := range items {
		switch {
		case item.ID <= 0:
			return fmt.Errorf("%s: an item has the ID %d, which isn't positive", path, item.ID)
		case computeShardIndexOfStarItem(item.ID) != shardIndex:
			return fmt.Errorf("%s: item %d belongs in %s", path, item.ID, BuildStarItemShardPath(computeShardIndexOfStarItem(item.ID)))
		case isSeen[item.ID]:
			return fmt.Errorf("%s: item %d appears twice", path, item.ID)
		}
		isSeen[item.ID] = true
	}
	return nil
}

// Makes the error for a shard that isn't in the DB repository.
// All shards always exist (`StarItemsDBDir`), so this means the repository is broken.
// It is a 502 like other answers from GitHub that can't be used, and nothing is read as an empty archive.
func newMissingStarItemShardError(path string) error {
	return Errorf(http.StatusBadGateway, "The archive file %s is missing in the DB repository", path)
}

// Decodes a JSON array (the content of a data file) into a list of items.
// `[T any]` is a type parameter, like a C++ template, so the same function works for each item type.
//
// Args:
//   - `content`: The file content. `nil` (no file) or `null` gives an empty list
func decodeJSONArray[T any](content []byte) ([]T, error) {
	items := []T{}
	if content != nil {
		if err := json.Unmarshal(content, &items); err != nil {
			return nil, err
		}
	}
	if items == nil { // A file that holds `null` sets the slice to `nil`
		items = []T{}
	}
	return items, nil
}

// Encodes a list of items as a JSON array with one item per line, the data file format.
// The result looks like `[\n{...},\n{...}\n]\n`, or `[]\n` if empty.
// All data file content is made by this function.
//
// Args:
//   - `items`: The items. The key order is the field order of the item struct
//
// NOTE: With one item per line, the diff can be read line by line, and commits made at the same time can be resolved with a rebase.
func encodeJSONArrayByLine[T any](items []T) ([]byte, error) {
	if len(items) == 0 {
		return []byte("[]\n"), nil
	}
	var buf bytes.Buffer
	var line bytes.Buffer // One item. It is emptied for each item and keeps its memory
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false) // Keeps `<`, `>`, `&` as they are instead of Unicode escapes, so the file reads as plain text
	buf.WriteString("[\n")
	for i, item := range items {
		if i > 0 {
			buf.WriteString(",\n")
		}
		line.Reset()
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
