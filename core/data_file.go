// Data files of the archive: the files that hold one item per line as a JSON array
// (`db/star-lists.json`, `db/repo-meta.json`, and the item shards under `StarItemsDBDir`).
// This file holds their format (`encodeJSONArrayByLine`, `FormatNow`), which path holds which item type (`dataFile`),
// and how a write tells whether a file changed (`trackedDataFile`).
// The archive items are split by ID into shards, each a data file of its own (`shardedStarItemsDataFile`).
// What the items mean is in `archive_stars.go` (items and star lists) and `archive_repo_meta.go`.
// README snapshots aren't data files. They are in `archive_readmes.go`.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// One data file of the archive: its path, and the Go type of its items (`T`).
// `[T any]` is a type parameter, like a C++ template.
// `T` isn't stored. It only fixes the item type that the methods decode.
// Reach a data file only through the values below, and a shard of the archive items only through `starItemsDataFile`.
// Then each path is always read as the type of its items.
type dataFile[T any] struct {
	path string // Path in the DB repository
}

// The data files of the archive
var (
	starListsDataFile = dataFile[StarList]{path: StarListsDBPath}
	repoMetaDataFile  = dataFile[RepoMeta]{path: RepoMetaDBPath}
)

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

// The archive items, split by ID into `StarItemShardCount` shard files under `StarItemsDBDir`.
// Each shard is a data file of its own, holding only the items of that shard (`computeShardIndexOfStarItem`).
// Its methods give the items of the shards they read as one list, like the items of one data file.
// A write may read only some shards (`decodeShardsForWriting`), and writes back only the shards that changed.
// The item type is fixed to `StarItem`, because splitting needs the item ID.
type shardedStarItemsDataFile struct {
	shards [StarItemShardCount]dataFile[StarItem] // Data file of each shard, by shard index
}

// The archive items.
// NOTE: It isn't one file like the other data files: it is the set of the shard files under `StarItemsDBDir` (`shardedStarItemsDataFile`).
var starItemsDataFile = newShardedStarItemsDataFile()

// Builds the data file of the archive items, with one data file per shard (`BuildStarItemShardPath`).
func newShardedStarItemsDataFile() shardedStarItemsDataFile {
	var f shardedStarItemsDataFile
	for index := range StarItemShardCount {
		f.shards[index] = dataFile[StarItem]{path: BuildStarItemShardPath(index)}
	}
	return f
}

// Builds the paths of all shards (`buildShardPaths`).
func (f shardedStarItemsDataFile) buildPaths() []string {
	return f.buildShardPaths(listAllStarItemShardIndexes())
}

// Builds the paths of the shards `shardIndexes`, to read them together with other files at one commit (`UpdateDBRepoFiles`).
func (f shardedStarItemsDataFile) buildShardPaths(shardIndexes []int) []string {
	paths := make([]string, 0, len(shardIndexes))
	for _, index := range shardIndexes {
		paths = append(paths, f.shards[index].path)
	}
	return paths
}

// Returns the indexes of all archive item shards, from 0 to `StarItemShardCount` - 1.
func listAllStarItemShardIndexes() []int {
	indexes := make([]int, 0, StarItemShardCount)
	for index := range StarItemShardCount {
		indexes = append(indexes, index)
	}
	return indexes
}

// Reads all shards at commit `commitSHA` into one list, for code that reads the items without writing.
// A write reads the shards through `UpdateDBRepoFiles` instead, together with its other files.
func (f shardedStarItemsDataFile) readFromDBRepoAtCommit(ctx context.Context, repo *DBRepo, commitSHA string) ([]StarItem, error) {
	filesAtCommit, err := readDBRepoFilesAtCommit(ctx, repo, f.buildPaths(), commitSHA)
	if err != nil {
		return nil, err
	}
	return f.decodeForReading(filesAtCommit)
}

// Reads all shards at the latest commit of the default branch into one list, for code that reads the items without writing.
// The items always come from one commit (`readLatestFromDBRepoIfChanged`).
func (f shardedStarItemsDataFile) readLatestFromDBRepo(ctx context.Context, repo *DBRepo) ([]StarItem, error) {
	read, err := f.readLatestFromDBRepoIfChanged(ctx, repo, "")
	return read.items, err
}

// Result of a read of the archive items that asks GitHub to answer only if a shard changed (`readLatestFromDBRepoIfChanged`)
type conditionalStarItemsRead struct {
	items         []StarItem // The items. `nil` if `isNotModified`
	etag          string     // GitHub's `ETag` of the listing of `StarItemsDBDir` the items came from. It changes only when a shard changes
	isNotModified bool       // `true` if GitHub answered 304 for the listing: no shard changed, and no shard was read
}

// Reads all shards at the latest commit of the default branch into one list,
// unless no shard changed since the listing of `StarItemsDBDir` whose GitHub `ETag` is `listingETag`.
// With an empty `listingETag`, it always reads.
//
// Returns:
//   - The items and the `ETag` of the listing they came from, or `isNotModified` with no items if no shard changed.
//     An error if the listing or a shard can't be read, or a shard is missing or breaks the layout (`checkStarItemShard`)
//
// NOTE: The listing decides which content each shard has, so the items always come from one commit.
// Only a shard missing from the listing is an error (502).
func (f shardedStarItemsDataFile) readLatestFromDBRepoIfChanged(ctx context.Context, repo *DBRepo, listingETag string) (conditionalStarItemsRead, error) {
	ref := "refs/heads/" + repo.branch // Named in full, because git looks up a short name as a tag before a branch (docs)
	var listing conditionalFolderListing
	var contents [StarItemShardCount][]byte
	errs := make([]error, StarItemShardCount+1) // One for each shard, and the last for the listing
	// With an `ETag`, asks for the listing alone first, and reads the shards only if it changed (one round trip more then)
	if listingETag != "" {
		var err error
		if listing, err = readBlobSHAsInFolderIfChanged(ctx, repo, ref, StarItemsDBDir, listingETag); err != nil || listing.isNotModified {
			return conditionalStarItemsRead{etag: listing.etag, isNotModified: listing.isNotModified}, err
		}
	}
	var wg sync.WaitGroup // Counter to wait until all started goroutines finish (similar to `std::thread::join`)
	// Without an `ETag`, asks for the listing and the shards at the same time, so the answer comes in about one round trip
	if listingETag == "" {
		wg.Add(1)
		go func() { // `go`: runs the function at the same time in a lightweight thread (goroutine)
			defer wg.Done()
			errs[StarItemShardCount] = RunWithPanicAsError(func() error { // A panic in a goroutine would end the whole process
				var err error
				listing, err = readBlobSHAsInFolderIfChanged(ctx, repo, ref, StarItemsDBDir, "")
				return err
			})
		}()
	}
	for index, shard := range f.shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = RunWithPanicAsError(func() error {
				var err error
				contents[index], err = readDBRepoFileAtRef(ctx, repo, shard.path, ref) // Each goroutine writes its own slot, so no lock is needed
				return err
			})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil { // `errors.Join`: combines several errors into one. `nil` if all are `nil`
		return conditionalStarItemsRead{}, err
	}
	for index, shard := range f.shards {
		blobSHA, isListed := listing.blobSHAByPath[shard.path]
		switch {
		case !isListed:
			contents[index] = nil // Missing at the commit of the listing, whatever the read gave
		case contents[index] == nil || computeGitBlobSHA(contents[index]) != blobSHA:
			// The read by branch name showed another commit, or gave a passing 404: reads the listed content by its blob sha
			var err error
			if contents[index], err = readDBRepoBlob(ctx, repo, blobSHA); err != nil {
				return conditionalStarItemsRead{}, err
			}
		}
	}
	items, err := f.decodeShardContents(contents)
	if err != nil {
		return conditionalStarItemsRead{}, err
	}
	return conditionalStarItemsRead{items: items, etag: listing.etag}, nil
}

// Decodes all shards from `filesAtCommit`, the files that `UpdateDBRepoFiles` read at one commit, into one list.
// It is for a write that only reads the items: changes to the result aren't written.
// A write that may change the items uses `decodeShardsForWriting`.
// It panics if a shard wasn't among the paths given to `UpdateDBRepoFiles` (`DBRepoFilesAtCommit.FileContent`).
func (f shardedStarItemsDataFile) decodeForReading(filesAtCommit *DBRepoFilesAtCommit) ([]StarItem, error) {
	var contents [StarItemShardCount][]byte
	for index, shard := range f.shards {
		contents[index] = filesAtCommit.FileContent(shard.path)
	}
	return f.decodeShardContents(contents)
}

// Decodes the shards `contents` (by shard index, `nil` for a shard that doesn't exist) into one list.
//
// Returns:
//   - The items, shard by shard in index order, and in file order within a shard.
//     An error if a shard can't be decoded, or is missing or breaks the layout (`checkStarItemShard`)
func (f shardedStarItemsDataFile) decodeShardContents(contents [StarItemShardCount][]byte) ([]StarItem, error) {
	items := []StarItem{}
	for index, shard := range f.shards {
		shardItems, err := decodeJSONArray[StarItem](contents[index])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shard.path, err)
		}
		if err := checkStarItemShard(index, contents[index], shardItems); err != nil {
			return nil, err
		}
		items = append(items, shardItems...)
	}
	return items, nil
}

// Decodes the shards `shardIndexes` from `filesAtCommit` into `*items` as one list, and starts tracking that variable.
// After the change, `addIfChanged` on the result adds to the commit only the shards whose items changed.
// With only some shards, `*items` holds only their items, and an item that belongs to another shard can't be written.
//
// Args:
//   - `filesAtCommit`: The files that `UpdateDBRepoFiles` read at one commit
//   - `shardIndexes`: The shards to decode, each once
//   - `items`: The variable to decode into. The write must change this variable, not a copy of it
//
// Returns:
//   - An error if a shard can't be decoded, or is missing or breaks the layout (`checkStarItemShard`).
//     `*items` is set only when every shard is good.
//     It panics if a shard wasn't among the paths given to `UpdateDBRepoFiles` (`DBRepoFilesAtCommit.FileContent`)
func (f shardedStarItemsDataFile) decodeShardsForWriting(filesAtCommit *DBRepoFilesAtCommit, shardIndexes []int, items *[]StarItem) (*trackedShardedStarItemsDataFile, error) {
	tracked := &trackedShardedStarItemsDataFile{items: items}
	decoded := []StarItem{}
	for _, index := range shardIndexes {
		shard := f.shards[index]
		var err error
		tracked.shards[index], err = shard.decodeForWriting(filesAtCommit, &tracked.shardItems[index])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", shard.path, err)
		}
		if err := checkStarItemShard(index, filesAtCommit.FileContent(shard.path), tracked.shardItems[index]); err != nil {
			return nil, err
		}
		decoded = append(decoded, tracked.shardItems[index]...)
	}
	*items = decoded
	return tracked, nil
}

// The archive items decoded from some or all shards into one variable for a change (`shardedStarItemsDataFile.decodeShardsForWriting`).
// Each shard is tracked as its own data file (`trackedDataFile`),
// and `addIfChanged` splits the one variable back into the shards before they are compared.
type trackedShardedStarItemsDataFile struct {
	shardItems [StarItemShardCount][]StarItem                 // Items of each shard. `shards` tracks these variables
	shards     [StarItemShardCount]*trackedDataFile[StarItem] // Each shard as read, to tell whether its items changed. `nil` for a shard that wasn't read
	items      *[]StarItem                                    // The variable the shards were decoded into. A pointer, so the latest value is seen even if the change replaces the slice
}

// Splits the items in the tracked variable into shards by ID,
// and adds to `changes` only the shards whose items now differ from the items as read (`trackedDataFile.addIfChanged`).
// Within each shard, the items keep their order in the variable.
// `changes.FilesToWrite` must not be `nil` (writing to a `nil` map panics in Go).
//
// Returns:
//   - An error, with nothing added, if an ID isn't positive, appears twice, or belongs to a shard that wasn't read.
//     An error if a shard can't be encoded
func (t *trackedShardedStarItemsDataFile) addIfChanged(changes *FileChangesCommit) error {
	var split [StarItemShardCount][]StarItem      // Items of each shard after the change
	isSeen := make(map[int64]bool, len(*t.items)) // Item ID -> `true` once seen
	for _, item := range *t.items {
		if item.ID <= 0 || isSeen[item.ID] { // The next read would refuse such a shard (`checkStarItemShard`)
			return fmt.Errorf("refusing to write star item %d: its ID isn't positive or appears twice", item.ID)
		}
		isSeen[item.ID] = true
		index := computeShardIndexOfStarItem(item.ID)
		if t.shards[index] == nil { // Writing that shard would replace its items with only the new ones
			return fmt.Errorf("refusing to write star item %d: its shard %s wasn't read", item.ID, BuildStarItemShardPath(index))
		}
		split[index] = append(split[index], item)
	}
	t.shardItems = split // The tracked variables of `shards` now hold the items after the change
	for index := range StarItemShardCount {
		if t.shards[index] == nil {
			continue // Not read, and none of its items is in `split`, as checked above
		}
		if err := t.shards[index].addIfChanged(changes); err != nil {
			return err
		}
	}
	return nil
}

// Checks shard `shardIndex`, read as `content` (`nil` if it doesn't exist) and decoded into `items`.
// The shard must exist, and every item must have a positive ID that belongs to this shard (`computeShardIndexOfStarItem`).
// No ID may appear twice.
//
// Returns:
//   - A 502 `HTTPError` if the shard doesn't exist, and an error if an item breaks the layout
//
// NOTE: An item in the wrong shard would show in the list, but a write would look for it only in its own shard.
// It could then never be changed or deleted, and Add would put a second copy in the right shard.
// So a shard that breaks the rule is refused as a whole, and a person fixes the file.
func checkStarItemShard(shardIndex int, content []byte, items []StarItem) error {
	path := BuildStarItemShardPath(shardIndex)
	if content == nil { // A missing file decodes as an empty list (`decodeJSONArray`), so it must not get past here
		// All shards always exist (`StarItemsDBDir`), so a missing one means the repository is broken.
		// It is a 502, like other answers from GitHub that can't be used
		return Errorf(http.StatusBadGateway, "The archive file %s is missing in the DB repository", path)
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
// All data file content that the server writes is made by this function.
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
