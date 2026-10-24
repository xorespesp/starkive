// Data files of the archive: the files that hold one item per line as a JSON array
// (`db/star-items.json`, `db/star-lists.json`, `db/repo-meta.json`).
// This file holds their format (`encodeJSONArrayByLine`, `FormatNow`), which path holds which item type (`dataFile`),
// and how a write tells whether a file changed (`trackedDataFile`).
// What the items mean is in `archive_stars.go` (items and star lists) and `archive_repo_meta.go`.
// README snapshots aren't data files. They are in `archive_readmes.go`.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// One data file of the archive: its path, and the Go type of its items (`T`).
// `[T any]` is a type parameter, like a C++ template.
// `T` isn't stored. It only fixes the item type that the methods decode.
// Reach a data file only through the values below.
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
