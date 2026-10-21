// Storage engine for files in the DB repository, and the archive data.
// It covers reading a file (`ReadFile`), listing a folder at a commit (`readBlobSHAsInFolder`),
// writing and deleting several files in one commit (`UpdateFiles`, Git Data API),
// handling the three archive files together with the README snapshots of their items (`UpdateArchive`), commit messages (`CommitMessage`),
// and the data file format (`encodeLines`).
// `db_repo.go` decides which repository to use, and whether it is safe to write to.
package core

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

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
	StarListIDs []int64 `json:"star_list_ids"`     // IDs of the star lists it belongs to. Always cleaned by `Archive.CleanStarListIDs`
	StarredAt   string  `json:"starred_at"`        // When it was starred on GitHub. For entries added by hand, when it was added
}

// Maximum number of tries for one save, the first try included.
// A save is tried again only when another commit got in first
const maxUpdateAttempts = 5

// Reads one file in the DB repository, at the latest commit of the default branch.
//
// Args:
//   - `path`: File path in the repository (e.g. `db/star-items.json`)
//
// Returns:
//   - The file content. `nil` if the file doesn't exist
func ReadFile(cfg *Config, path string) ([]byte, error) {
	return readFileAt(cfg, path, cfg.DBRepoBranch)
}

// Reads one file as it was at a given point.
//
// Args:
//   - `path`: File path in the repository
//   - `ref`: The point to read at. A branch name or a commit sha
//
// Returns:
//   - The file content. `nil` if the file doesn't exist (404)
//
// NOTE: The default Contents API response leaves the content empty for files over 1MB (docs).
// Archive files can grow larger than that, so they are fetched in raw format.
func readFileAt(cfg *Config, path string, ref string) ([]byte, error) {
	res, err := CallGitHubAPI(cfg, BuildContentsPath(cfg, path)+"?ref="+url.QueryEscape(ref),
		GitHubAPIRequest{Accept: "application/vnd.github.raw+json"})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if err := CheckGitHubAPI(res); err != nil {
		return nil, err
	}
	return io.ReadAll(res.Body)
}

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

// Reads the data file `path` and decodes it into a list of items. Used when only reading, not writing
func readItems[T any](cfg *Config, path string) ([]T, error) {
	content, err := ReadFile(cfg, path)
	if err != nil {
		return nil, err
	}
	return decodeItems[T](content)
}

// All archive data. `UpdateArchive` reads the three files together from one commit and puts them here
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

// Error for a write that has files to write or delete but a commit message that isn't valid (`isValid`).
// Commits that don't say what they changed are never made
var errInvalidCommitMessage = errors.New("refusing to commit changes without a valid commit message")

// Reports whether `p` is a clean path relative to the repository root (e.g. `db/star-items.json`).
// One file then has only one spelling, so two different paths never name the same file.
func isValidRepoPath(p string) bool {
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || strings.Contains(p, `\`) {
		return false
	}
	return path.Clean(p) == p // `path.Clean` removes `//`, `./`, `x/..` and a trailing `/`
}

// Contents of one commit to make: the files to write and delete, and the commit message
type FileChangesCommit struct {
	FilesToWrite  map[string][]byte // Path -> new content. A path doesn't have to be one of the files read
	FilesToDelete []string          // Paths of files to delete. Each must exist at the commit the files were read from, or the whole commit fails (measured)
	Message       CommitMessage     // Commit message. Required if anything is written or deleted
}

// Reports whether there is nothing to write or delete, so no commit is needed
func (changes FileChangesCommit) isEmpty() bool {
	return len(changes.FilesToWrite) == 0 && len(changes.FilesToDelete) == 0
}

// Checks that the commit can be made as it is; it changes something, its message is valid, and its paths are clean.
// A path may appear only once, because writing and deleting the same file in one commit has no single meaning.
func (changes FileChangesCommit) check() error {
	if changes.isEmpty() {
		return errors.New("refusing to make a commit that changes nothing")
	}
	if !changes.Message.isValid() {
		return errInvalidCommitMessage
	}
	for p := range changes.FilesToWrite {
		if !isValidRepoPath(p) {
			return fmt.Errorf("refusing to write a file path that isn't valid: %q", p)
		}
	}
	pathsSeen := map[string]bool{} // Paths to delete checked so far
	for _, p := range changes.FilesToDelete {
		if !isValidRepoPath(p) {
			return fmt.Errorf("refusing to delete a file path that isn't valid: %q", p)
		}
		_, alsoWritten := changes.FilesToWrite[p] // `v, ok := m[k]`: `ok` is false if the key is missing
		if alsoWritten || pathsSeen[p] {
			return fmt.Errorf("refusing to change the file path %q twice in one commit", p)
		}
		pathsSeen[p] = true
	}
	return nil
}

// Reads several files in the DB repository from one commit, and writes what `buildFileChangesCommit` returns as one commit.
// Every write to the DB repository goes through this function.
// The only exception is the first commit in an empty repository.
// The Git Data API doesn't work in an empty repository, so the setup tool writes that commit with the Contents API.
//
// Args:
//   - `paths`: Paths of the files to read. Each must pass `isValidRepoPath`
//   - `buildFileChangesCommit`: Takes `filesRead`, the files read from one commit (path -> content, `nil` if the file doesn't exist),
//     and `commitSHA`, the sha of that commit.
//     It can read more at the same commit with `commitSHA` (for example a folder listing, `readBlobSHAsInFolder`).
//     Paths found that way exist in the tree the new commit is built on, so they are safe to put in `FilesToDelete`.
//     It returns the commit to make (`FileChangesCommit`).
//     If nothing is written or deleted, or the changes leave every file as it was, nothing is committed.
//     Otherwise the commit must pass `FileChangesCommit.check`.
//     If it returns an error, nothing is written
//
// Returns:
//   - An error from `buildFileChangesCommit` or from GitHub.
//     An error from `FileChangesCommit.check` (`errInvalidCommitMessage` for a message that isn't valid). Then nothing is written.
//     409 `errBranchMoved` if other commits keep getting in until all retries are used up
//
// NOTE:
//   - Even when several files change, there is only one commit.
//     So a change like "delete a star list and clean up its members" has no in-between state.
//   - If another commit (another device, a sync) lands after the read, GitHub refuses to move the branch (`errBranchMoved`).
//     Then it starts over from the new commit.
//     This is the only case that is retried.
//     Errors while making the tree or the commit are not conflicts, so they are returned right away.
//   - `buildFileChangesCommit` can be called more than once.
//     If it leaves results in outer variables, it must set them from scratch on every call.
//   - One attempt costs "5 + number of files read" GitHub requests (branch, its commit, one per file read, tree, commit, branch move),
//     plus the requests that `buildFileChangesCommit` sends.
//     For the archive (three files) that is 8 when `buildFileChangesCommit` sends none.
//     Files that are only written or deleted cost no extra request, except one per written file that needs a blob (`isTreeContentSafe`).
//     This is the price of putting several files in one commit with no in-between state.
func UpdateFiles(cfg *Config, paths []string, buildFileChangesCommit func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error)) error {
	for _, p := range paths {
		if !isValidRepoPath(p) {
			return fmt.Errorf("refusing to read a file path that isn't valid: %q", p)
		}
	}
	// Compare-and-swap loop: the commit is built on the branch tip that was read, and the branch moves only if it is still there.
	// If another commit (another device, a sync) got in first, the files are read and the commit is built again on the new tip
	for attempt := 1; ; attempt++ {
		head, err := readBranchHead(cfg)
		if err != nil {
			return err
		}
		filesRead, err := readFilesAt(cfg, paths, head.commit)
		if err != nil {
			return err
		}
		changes, err := buildFileChangesCommit(filesRead, head.commit)
		if err != nil {
			return err
		}
		if changes.isEmpty() { // Nothing to commit, so the message isn't needed either
			return nil
		}
		if err := changes.check(); err != nil {
			return err
		}

		err = commitFiles(cfg, head, changes)
		if err == nil {
			return nil
		}
		if errors.Is(err, errBranchMoved) && attempt < maxUpdateAttempts { // The only case that is retried is a moved branch
			// Right after another commit, a branch read can briefly point to the old commit.
			// So wait a little longer each time before reading again
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
			continue
		}
		return err
	}
}

// Reads several files at the same commit, in parallel.
//
// Args:
//   - `paths`: Paths of the files to read
//   - `ref`: The commit sha to read at. All files must be read at the same commit so they match each other
//
// Returns:
//   - Path -> content (`nil` if the file doesn't exist). An error if any file can't be read
func readFilesAt(cfg *Config, paths []string, ref string) (map[string][]byte, error) {
	contents := make([][]byte, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup // Counter to wait until all started goroutines finish (similar to `std::thread::join`)
	for i, path := range paths {
		wg.Add(1)
		go func() { // `go`: runs the function at the same time in a lightweight thread (goroutine)
			defer wg.Done()
			contents[i], errs[i] = readFileAt(cfg, path, ref) // Each goroutine writes its own slot, so no lock is needed
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil { // `errors.Join`: combines several errors into one. `nil` if all are `nil`
		return nil, err
	}
	files := make(map[string][]byte, len(paths))
	for i, path := range paths {
		files[path] = contents[i]
	}
	return files, nil
}

// Reads the git blob sha of every file under folder `folderPath` at commit `commitSHA`, with one request.
// Files in subfolders are included. File contents aren't read.
//
// Args:
//   - `commitSHA`: The commit sha to read at
//   - `folderPath`: Folder path in the repository, clean and without a trailing `/` (e.g. `db/readmes/45`)
//
// Returns:
//   - Path in the repository -> git blob sha of the file content. An empty map if the folder doesn't exist.
//     A 502 `HTTPError` if GitHub cut the list short, because a short list would make present files look absent
//
// NOTE: The tree API takes `<commit sha>:<folder path>` in place of a tree sha (measured).
// A list of 74,000 files (23MB) was not cut short (measured).
func readBlobSHAsInFolder(cfg *Config, commitSHA string, folderPath string) (map[string]string, error) {
	if !isValidRepoPath(folderPath) {
		return nil, fmt.Errorf("refusing to list a folder path that isn't valid: %q", folderPath)
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"` // Path relative to `folderPath`
			Type string `json:"type"` // `blob` for a file, `tree` for a folder
			SHA  string `json:"sha"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	escapedFolderPath := (&url.URL{Path: folderPath}).EscapedPath()
	err := callRepoAPI(cfg, http.MethodGet, "/git/trees/"+commitSHA+":"+escapedFolderPath+"?recursive=1", nil, &tree)
	if hasGitHubStatus(err, http.StatusNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if tree.Truncated {
		return nil, Errorf(http.StatusBadGateway, "GitHub cut short the file list of %s", folderPath)
	}
	blobSHAByPath := map[string]string{}
	for _, entry := range tree.Tree {
		if entry.Type == "blob" {
			blobSHAByPath[folderPath+"/"+entry.Path] = entry.SHA
		}
	}
	return blobSHAByPath, nil
}

// Returns the git blob sha of file content `content`, the value GitHub gives as the sha of a file.
// It is the SHA-1 of `blob <size>` and a NUL byte, followed by the content (docs).
// Comparing it with a listed sha tells whether a file already holds `content`, without reading the file.
func gitBlobSHA(content []byte) string {
	hash := sha1.New() // `sha1.New`: a hash that takes its input in parts, like a streaming hash object in C++
	fmt.Fprintf(hash, "blob %d%c", len(content), 0)
	hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

// Reports whether `content` can go into a tree entry as a JSON string as it is.
// JSON strings hold only UTF-8.
// A NUL byte is left out too, because how GitHub stores `\u0000` in tree content isn't measured
func isTreeContentSafe(content []byte) bool {
	return utf8.Valid(content) && bytes.IndexByte(content, 0) < 0
}

// Number of blobs created at the same time for one commit (`createBlobs`).
// A commit can hold hundreds of files that need a blob, and one at a time would take minutes
const blobCreateParallel = 6

// Creates a blob from base64 for each of `paths`, with the content in `contentByPath`, `blobCreateParallel` at a time.
//
// Returns:
//   - Path -> blob sha. Empty if `paths` is empty, and then no request is sent
//   - The first error from GitHub. Blobs created before it are left unused, which is harmless
func createBlobs(cfg *Config, paths []string, contentByPath map[string][]byte) (map[string]string, error) {
	// Several goroutines (lightweight threads) take paths one by one. Shared values are read and written while holding `mu`
	var mu sync.Mutex
	next := 0
	blobSHAByPath := map[string]string{}
	var firstErr error
	var wg sync.WaitGroup // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < blobCreateParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if firstErr != nil || next >= len(paths) {
					mu.Unlock()
					return
				}
				path := paths[next]
				next++
				mu.Unlock()

				var blob struct {
					SHA string `json:"sha"`
				}
				request := map[string]any{"content": base64.StdEncoding.EncodeToString(contentByPath[path]), "encoding": "base64"}
				err := callRepoAPI(cfg, http.MethodPost, "/git/blobs", request, &blob)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				blobSHAByPath[path] = blob.SHA
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return blobSHAByPath, nil
}

// Reads the three archive files from one commit, changes them with `change`, and writes only the changed files in one commit.
// It works on top of `UpdateFiles`.
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
func UpdateArchive(cfg *Config, change func(a *Archive) (CommitMessage, error)) (*Archive, error) {
	// Turns the three data files into file content (the `encodeLines` format). Returns path -> content
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
	err := UpdateFiles(cfg, []string{StarItemsDBPath, StarListsDBPath, RepoMetaDBPath}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
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
			readmePaths, err := readReadmeSnapshotPaths(cfg, commitSHA, id)
			if err != nil {
				return FileChangesCommit{}, err
			}
			changes.FilesToDelete = append(changes.FilesToDelete, readmePaths...)
		}

		// Saves the READMEs given to `AddFetchedReadmeToSave`, against the snapshots already saved for those items
		for id, fetched := range a.fetchedReadmesToSave {
			if !isArchivedAfter[id] {
				continue // A README of an item not in the archive would have no item to own it, and nothing would delete it
			}
			snapshotsByID, err := readReadmeSnapshotsInFolder(cfg, commitSHA, buildReadmeFolderPath(id))
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

// Error for a branch that couldn't be moved because another commit landed after the read (returned by `commitFiles`).
// It is a prebuilt error value, so `errors.Is` compares against it (similar to an error code constant in C++).
//
// NOTE: `UpdateFiles` retries only on this error.
// When all retries are used up, it returns this error as is, as a 409.
// The signal "try again" and the message for the user ("the archive changed while saving") are the same fact, so they share one value.
var errBranchMoved = Errorf(http.StatusConflict, "The archive changed while saving. Try again")

// The commit at the tip of a branch, and that commit's file tree
type branchHead struct {
	commit string // Commit sha
	tree   string // Tree sha of that commit. Used as the base when making a new tree
}

// Reads the commit sha that the default branch points to.
//
// Returns:
//   - The commit sha. For an empty repository, GitHub refuses with 409 (`GitHubAPIError`)
func readBranchCommit(cfg *Config) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := callRepoAPI(cfg, http.MethodGet, "/git/ref/heads/"+cfg.DBRepoBranch, nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

// Reads the commit that the default branch points to, and its tree
func readBranchHead(cfg *Config) (branchHead, error) {
	sha, err := readBranchCommit(cfg)
	if err != nil {
		return branchHead{}, err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := callRepoAPI(cfg, http.MethodGet, "/git/commits/"+sha, nil, &commit); err != nil {
		return branchHead{}, err
	}
	return branchHead{commit: sha, tree: commit.Tree.SHA}, nil
}

// Writes and deletes several files in one commit (GitHub Git Data API):
//  1. Makes a new tree from the tree of `head`, with only the changed files replaced or removed
//  2. Makes a new commit with `head` as its parent
//  3. Moves the branch to the new commit (without force)
//
// Args:
//   - `head`: The branch tip at the time the files were read. It becomes the parent of the new commit
//   - `changes`: Files to write and delete, and the commit message
//
// Returns:
//   - `nil` on success, and also when the new tree equals the tree of `head` (then no commit is made).
//     `errBranchMoved` if the branch moved in the meantime. Otherwise the GitHub error
//
// NOTE:
//   - A tree entry carries its content as a JSON string, which holds only UTF-8.
//     So a file that can't go in a JSON string as it is (`isTreeContentSafe`) is first made into a blob from base64 (docs),
//     and its entry carries the blob sha.
//     That costs one more request for each such file, sent `blobCreateParallel` at a time (`createBlobs`).
//   - A tree entry with a `null` sha and no content deletes that path (measured).
//     A folder left empty disappears with it (measured).
//     Deleting a path that isn't there fails the whole tree with 422 (measured).
//   - Step 3 doesn't use force.
//     So if the branch moved to another commit in the meantime, GitHub refuses (the same idea as compare-and-swap in C++).
//     The tree and commit made earlier are not linked to the branch, so they are simply dropped.
//   - If step 3 fails, the cause is not guessed from the status code.
//     Instead, the branch is read again.
//     GitHub also answers 422 or 409 when the branch doesn't exist or the repository is empty.
//     And even with no response, the move may have happened.
//     The only test is "does the branch now contain the new commit".
//     If it does, it succeeded (only the response was lost).
//     If it doesn't and the branch moved, it is a conflict.
//     If the branch didn't move, the GitHub error is returned.
//     Testing "does the branch point to the new commit" would be wrong.
//     If one more commit lands after a lost response, a change that is already in would be applied again.
//   - Limit: right after the move, GitHub may briefly return the old branch value.
//     Then it looks like "didn't move" and the error is returned.
//     The data is safe, and the request can simply be sent again.
func commitFiles(cfg *Config, head branchHead, changes FileChangesCommit) error {
	entries := []map[string]any{} // Tree entries. `any` lets one entry hold a string and another a JSON `null`
	blobPaths := []string{}       // Paths of the files that go through a blob
	for path, content := range changes.FilesToWrite {
		if isTreeContentSafe(content) {
			entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "content": string(content)})
		} else {
			blobPaths = append(blobPaths, path)
		}
	}
	blobSHAByPath, err := createBlobs(cfg, blobPaths, changes.FilesToWrite)
	if err != nil {
		return err
	}
	for _, path := range blobPaths {
		entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "sha": blobSHAByPath[path]})
	}
	for _, path := range changes.FilesToDelete {
		entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "sha": nil}) // `nil` becomes JSON `null`
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := callRepoAPI(cfg, http.MethodPost, "/git/trees", map[string]any{"base_tree": head.tree, "tree": entries}, &tree); err != nil {
		return err
	}
	if tree.SHA == head.tree { // Every change matched what was already there, so a commit would change nothing
		return nil
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := callRepoAPI(cfg, http.MethodPost, "/git/commits", map[string]any{"message": changes.Message.String(), "tree": tree.SHA, "parents": []string{head.commit}}, &commit); err != nil {
		return err
	}
	// Moves without force. If the branch moved in the meantime, GitHub refuses
	err = callRepoAPI(cfg, http.MethodPatch, "/git/refs/heads/"+cfg.DBRepoBranch, map[string]any{"sha": commit.SHA, "force": false}, nil)
	if err == nil {
		return nil
	}
	// Doesn't guess the cause from the status code.
	// Reads the branch again and checks whether it contains the new commit
	now, readErr := readBranchCommit(cfg)
	if readErr != nil || now == head.commit { // The branch didn't move: not a conflict
		return err
	}
	included, compareErr := isCommitInBranch(cfg, now, commit.SHA)
	switch {
	case compareErr != nil:
		return err
	case included: // The move happened, only the response was lost
		return nil
	}
	return errBranchMoved // Another commit landed in the meantime
}

// Asks GitHub whether a branch contains a commit.
// That is, whether following parents back from the branch tip reaches that commit.
//
// Args:
//   - `branchCommit`: The commit sha that the branch tip points to now
//   - `newCommit`: The commit sha made for this save
//
// Returns:
//   - `true` if it contains it (also when it is the same commit)
//
// NOTE: The compare API (`newCommit...branchCommit`) answers "ahead" or "identical" when `branchCommit` contains `newCommit`.
func isCommitInBranch(cfg *Config, branchCommit string, newCommit string) (bool, error) {
	var compare struct {
		Status string `json:"status"` // ahead, identical, behind, diverged
	}
	if err := callRepoAPI(cfg, http.MethodGet, "/compare/"+newCommit+"..."+branchCommit, nil, &compare); err != nil {
		return false, err
	}
	return compare.Status == "ahead" || compare.Status == "identical", nil
}

// Calls a DB repository API (under `/repos/<repository>`: Git Data, compare, Actions, etc.) and fills a value from the JSON response.
//
// Args:
//   - `method`: HTTP method
//   - `path`: The path after `/repos/<repository>` (e.g. `/git/trees`)
//   - `body`: Body to send as JSON. `nil` if there is none
//   - `dst`: Pointer to the value to fill from the JSON response. If `nil`, the response body is discarded
//
// Returns:
//   - `GitHubAPIError` if GitHub answers with a failure
func callRepoAPI(cfg *Config, method string, path string, body any, dst any) error {
	res, err := CallGitHubAPI(cfg, "/repos/"+cfg.DBRepoFullName+path, GitHubAPIRequest{Method: method, Body: body})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(dst)
}

// Maximum length of a commit message, in characters.
// `formatCommitList` already shortens lists in the body, so this is only a safety net for messages that are still too long
const maxCommitMessage = 4000

// Maximum number of items listed in a commit body. The rest is shortened to "… and N more"
const maxListedItems = 20

// Builds one list block for a commit body: a title line and `- item` lines.
//
// Args:
//   - `title`: Title of the list (e.g. "New star lists"). If empty, no title line is written
//   - `items`: Items to list
//
// Returns:
//   - The list text. An empty string if there are no items (`NewCommitMessage` skips it)
//
// NOTE: A sync or an Import can have thousands of items.
// So only up to `maxListedItems` items are listed, and the rest is given as a count.
// The diff shows the actual changes.
func formatCommitList(title string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	lines := []string{}
	if title != "" {
		lines = append(lines, title+":")
	}
	for _, item := range items[:min(len(items), maxListedItems)] {
		lines = append(lines, "- "+item)
	}
	if len(items) > maxListedItems {
		lines = append(lines, fmt.Sprintf("… and %d more", len(items)-maxListedItems))
	}
	return strings.Join(lines, "\n")
}

// Message of a DB repository commit: one subject line and a body below it. Made with `NewCommitMessage`
type CommitMessage struct {
	subject string   // Subject in the form `<prefix>: <summary>` (`NewCommitMessage`)
	body    []string // Body blocks, added below the subject with blank lines between them
}

// Builds a commit message.
// The subject is `<prefix>: <summary>`, so commits can be filtered by who made them, like `git log --grep "^sync("`.
//
// Args:
//   - `prefix`: Who made the change and which file changed, written like `user(star-items)`.
//     Who is `user` (something a person did in the dashboard or the setup tool) or `sync` (auto sync).
//     The file is `star-items`, `star-lists`, `repo-meta`, `config`, or `workflow`, or the folder `readmes` (`ReadmesDBDir`).
//     The one exception is the commit that adds the initial files of the DB repository, whose prefix is `feat` (`feat: Add initial files`)
//   - `summary`: What changed, starting with a verb (e.g. "add owner/repo")
//   - `body`: Body blocks (use `formatCommitList` for a list of many items). Empty blocks are skipped
//
// NOTE: The fields are private, so other packages can make a message only with this function, or as the zero value (which isn't valid).
// That way, every commit has the same subject and body format.
func NewCommitMessage(prefix string, summary string, body ...string) CommitMessage {
	m := CommitMessage{subject: prefix + ": " + summary}
	for _, section := range body {
		if section != "" {
			m.body = append(m.body, section)
		}
	}
	return m
}

// Reports whether the subject has the form `<prefix>: <summary>`, with neither part empty or only spaces.
// The zero value (`CommitMessage{}`) doesn't.
func (m CommitMessage) isValid() bool {
	prefix, summary, found := strings.Cut(m.subject, ": ")
	return found && strings.TrimSpace(prefix) != "" && strings.TrimSpace(summary) != ""
}

// Turns the message into the text for a commit (subject, blank line, body blocks separated by blank lines).
// If it is longer than `maxCommitMessage` characters, the end is cut off (the subject comes first, so it is never cut)
func (m CommitMessage) String() string {
	text := strings.Join(append([]string{m.subject}, m.body...), "\n\n")
	if runes := []rune(text); len(runes) > maxCommitMessage { // Cuts by character, not by byte, so multi-byte characters are not broken
		text = string(runes[:maxCommitMessage])
	}
	return text
}

// Joins a count and an English noun in singular or plural form (1 star, 3 stars).
//
// Args:
//   - `n`: The count
//   - `noun`: The noun in singular form. The plural adds `s` at the end
func FormatCount(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
