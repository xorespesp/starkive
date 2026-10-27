// Storage engine for the files in the DB repository (GitHub Git Data API).
// It covers reading a file (`ReadDBRepoFile`), listing a folder at a commit (`readBlobSHAsInFolder`),
// and writing and deleting several files in one commit (`UpdateDBRepoFiles`).
// It writes the bytes it is given.
// What they mean is decided by the archive (`data_file.go` and the `archive_*.go` files) and the config file (`db_config.go`).
// `db_repo.go` also decides which repository to use, and whether it is safe to write to.
package core

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Reads one file in the DB repository, at the latest commit of the default branch.
//
// Args:
//   - `path`: File path in the repository (e.g. `db/star-lists.json`)
//
// Returns:
//   - The file content. `nil` if the file doesn't exist
func ReadDBRepoFile(ctx context.Context, repo *DBRepo, path string) ([]byte, error) {
	return readDBRepoFileAtRef(ctx, repo, path, repo.branch)
}

// Reads one file at `ref`, a branch name or a commit sha, through the cache of the request (`readDBRepoFileThroughCache`).
//
// Returns:
//   - The file content. `nil` if the file doesn't exist (404).
//     Errors are those of `readDBRepoFileAtRefIfChanged`
func readDBRepoFileAtRef(ctx context.Context, repo *DBRepo, path string, ref string) ([]byte, error) {
	content, _, err := readDBRepoFileThroughCache(ctx, repo, path, ref)
	return content, err
}

// Reads one file at `ref` through the cache of the request in `ctx` (`dbRepoFileCache`).
// A file that the request already read or wrote (`keepCommittedDBRepoFiles`) is asked for with its `ETag`.
// So reading it again (a retry after a conflict, the next step of a sync) downloads nothing if it didn't change,
// and the content is still the one at `ref`.
//
// Returns:
//   - The content (`nil` if the file doesn't exist), or the error of `readDBRepoFileAtRefIfChanged`.
//     `true` if GitHub answered 304 and the content is the one kept in the cache
func readDBRepoFileThroughCache(ctx context.Context, repo *DBRepo, path string, ref string) ([]byte, bool, error) {
	cache := readDBRepoFileCache(ctx)
	kept, isKept := cache.get(path)
	read, err := readDBRepoFileAtRefIfChanged(ctx, repo, path, ref, kept.etag)
	if err != nil {
		return nil, false, err
	}
	if isKept && read.isNotModified {
		return kept.content, true, nil
	}
	if read.content != nil {
		cache.put(path, read.etag, read.content)
	}
	return read.content, false, nil
}

// Result of a read that asks GitHub to answer only if the resource changed (`If-None-Match`)
type conditionalDBRepoRead struct {
	content       []byte // The content. `nil` if the file doesn't exist, or if `isNotModified`
	etag          string // GitHub's `ETag` of the content, to ask with next time. Empty if the file doesn't exist or GitHub gave none
	isNotModified bool   // `true` if GitHub answered 304: the content is the same as the one whose `ETag` was asked with
}

// Reads one file at `ref`, unless its content is the same as the one whose GitHub `ETag` is `ifNoneMatch`.
// With an empty `ifNoneMatch`, it is a plain read.
//
// Returns:
//   - The content and its `ETag`, or `isNotModified` with no content if GitHub answered 304 (the content didn't change).
//     A file that doesn't exist (404) is no content and no `ETag`.
//     A `GitHubAPIError` if GitHub answers with another failure (wrapped with `ErrGitHubRateLimited` for a rate limit answer)
//
// NOTE: GitHub answers 304 if the content is the same, also at another commit or by branch name,
// and a 304 doesn't count toward the rate limit (measured).
// The `ETag` of a file is its git blob sha in quotes (measured), but a read only sends it back.
func readDBRepoFileAtRefIfChanged(ctx context.Context, repo *DBRepo, path string, ref string, ifNoneMatch string) (conditionalDBRepoRead, error) {
	res, err := CallGitHubAPI(ctx, repo.cfg, repo.BuildContentsAPIPath(path)+"?ref="+url.QueryEscape(ref),
		GitHubAPIRequest{Accept: "application/vnd.github.raw+json", IfNoneMatch: ifNoneMatch}) // Raw format, so files over 1MB come too (docs)
	if err != nil {
		return conditionalDBRepoRead{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return conditionalDBRepoRead{}, nil
	}
	if ifNoneMatch != "" && res.StatusCode == http.StatusNotModified {
		return conditionalDBRepoRead{etag: ifNoneMatch, isNotModified: true}, nil
	}
	if err := CheckGitHubResponseWithRateLimit(res); err != nil {
		return conditionalDBRepoRead{}, err
	}
	content, err := io.ReadAll(res.Body)
	if err != nil { // The body stopped coming (a dropped connection, `githubTimeout` or the deadline of `ctx`)
		return conditionalDBRepoRead{}, newGitHubUnreachableError(err)
	}
	return conditionalDBRepoRead{content: content, etag: res.Header.Get("ETag")}, nil
}

// Files read from one commit of the DB repository for a write (`UpdateDBRepoFiles`): the commit, and the content of each path asked for.
// Only `readDBRepoFilesAtCommit` makes it, and every path it was asked for is a key of `contents`.
// The fields aren't exported, so other packages can only make an empty one, which panics on any path.
//
// NOTE: Asking for a path that wasn't read panics (`FileContent`).
// A file that wasn't read would otherwise look like a file that doesn't exist.
// A write that adds a file "if it isn't there" would then overwrite one that is.
// Asking for it is a mistake in the code, and the panic shows it the first time that code runs.
// In the server it ends only that request with 500 (`ServeAPIRequest`), and nothing is written.
type DBRepoFilesAtCommit struct {
	commitSHA      string            // The commit the files were read at
	contents       map[string][]byte // Path -> content at the commit. `nil` if the file doesn't exist there
	fromCacheCount int               // How many of the files came from the cache of the request (GitHub answered 304). Only shown in `Server-Timing`
}

// Returns the sha of the commit the files were read at.
// More can be read at the same commit with it (for example a folder listing, `readBlobSHAsInFolder`)
func (files *DBRepoFilesAtCommit) CommitSHA() string { return files.commitSHA }

// Returns the content of the file `path` at the commit.
// `nil` if the file doesn't exist there.
// It panics if `path` wasn't among the paths asked for.
func (files *DBRepoFilesAtCommit) FileContent(path string) []byte {
	content, requested := files.contents[path] // "comma ok": `requested` tells whether the key is in the map at all
	if !requested {
		panic(fmt.Sprintf("%s wasn't read at commit %s", path, files.commitSHA))
	}
	return content
}

// Reports whether the file `path` existed at the commit.
// It panics if `path` wasn't among the paths asked for (`FileContent`).
func (files *DBRepoFilesAtCommit) HasFile(path string) bool { return files.FileContent(path) != nil }

// Reads the files `paths` at commit `commitSHA`, in parallel.
// All files are read at the same commit, so they match each other.
//
// Returns:
//   - The files. Every path in `paths` is a key, with `nil` for a file that doesn't exist.
//     An error if any file can't be read
func readDBRepoFilesAtCommit(ctx context.Context, repo *DBRepo, paths []string, commitSHA string) (*DBRepoFilesAtCommit, error) {
	contents := make([][]byte, len(paths))
	isFromCache := make([]bool, len(paths))
	errs := make([]error, len(paths))
	var wg sync.WaitGroup // Counter to wait until all started goroutines finish (similar to `std::thread::join`)
	for i, path := range paths {
		wg.Go(func() { // `wg.Go`: runs the function at the same time in a lightweight thread (goroutine), and `wg.Wait` waits for it
			errs[i] = RunWithPanicAsError(func() error { // A panic in a goroutine would end the whole process
				var err error
				contents[i], isFromCache[i], err = readDBRepoFileThroughCache(ctx, repo, path, commitSHA) // Each goroutine writes its own slot, so no lock is needed
				return err
			})
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil { // `errors.Join`: combines several errors into one. `nil` if all are `nil`
		return nil, err
	}
	filesAtCommit := &DBRepoFilesAtCommit{commitSHA: commitSHA, contents: make(map[string][]byte, len(paths))}
	for i, path := range paths {
		filesAtCommit.contents[path] = contents[i]
		if isFromCache[i] {
			filesAtCommit.fromCacheCount++
		}
	}
	return filesAtCommit, nil
}

// Reads the git blob sha of every file under folder `folderPath` at `ref`, with one request.
// Files in subfolders are included.
// File contents aren't read.
//
// Args:
//   - `ref`: The point to read at. A commit sha, or a branch named in full (`refs/heads/...`)
//   - `folderPath`: Folder path in the repository, clean and without a trailing `/` (e.g. `db/readmes/45`)
//
// Returns:
//   - Path in the repository -> git blob sha of the file content, as `readBlobSHAsInFolderIfChanged` gives it, or its error
func readBlobSHAsInFolder(ctx context.Context, repo *DBRepo, ref string, folderPath string) (map[string]string, error) {
	listing, err := readBlobSHAsInFolderIfChanged(ctx, repo, ref, folderPath, "")
	return listing.blobSHAByPath, err
}

// Result of a folder listing that asks GitHub to answer only if the listing changed (`If-None-Match`)
type conditionalFolderListing struct {
	blobSHAByPath map[string]string // Path in the repository -> git blob sha of the file content. Empty if the folder doesn't exist. `nil` if `isNotModified`
	etag          string            // GitHub's `ETag` of the listing, to ask with next time. Empty if the folder doesn't exist or GitHub gave none
	isNotModified bool              // `true` if GitHub answered 304: no file under the folder changed since the listing of that `ETag`
}

// Reads the listing of folder `folderPath` at `ref` like `readBlobSHAsInFolder`,
// unless it is the same as the listing whose GitHub `ETag` is `ifNoneMatch`.
// With an empty `ifNoneMatch`, it is a plain listing.
//
// Returns:
//   - The listing and its `ETag`, or `isNotModified` with no listing if GitHub answered 304.
//     An error for a `folderPath` that isn't valid.
//     A 502 `HTTPError` if GitHub cut the list short, because a short list would make present files look absent
//
// NOTE: The `ETag` of a listing isn't the tree sha of the folder (measured), so the code treats it only as a value to send back.
// GitHub answers 304 while no file under the folder changes, also at another commit or by branch name,
// and a 304 doesn't count toward the rate limit (measured).
func readBlobSHAsInFolderIfChanged(ctx context.Context, repo *DBRepo, ref string, folderPath string, ifNoneMatch string) (conditionalFolderListing, error) {
	if !isValidRepoPath(folderPath) {
		return conditionalFolderListing{}, fmt.Errorf("refusing to list a folder path that isn't valid: %q", folderPath)
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
	// The tree API takes `<commit sha>:<folder path>` or `<branch>:<folder path>` in place of a tree sha (measured)
	res, err := CallGitHubAPI(ctx, repo.cfg, repo.BuildAPIPath("/git/trees/"+url.PathEscape(ref)+":"+escapedFolderPath+"?recursive=1"),
		GitHubAPIRequest{IfNoneMatch: ifNoneMatch})
	if err != nil {
		return conditionalFolderListing{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return conditionalFolderListing{blobSHAByPath: map[string]string{}}, nil
	}
	if ifNoneMatch != "" && res.StatusCode == http.StatusNotModified {
		return conditionalFolderListing{etag: ifNoneMatch, isNotModified: true}, nil
	}
	if err := CheckGitHubResponseWithRateLimit(res); err != nil {
		return conditionalFolderListing{}, err
	}
	if err := DecodeGitHubResponse(res, &tree); err != nil {
		return conditionalFolderListing{}, err
	}
	if tree.Truncated { // GitHub didn't cut short even a list of 74,000 files (23MB, measured)
		return conditionalFolderListing{}, Errorf(http.StatusBadGateway, "GitHub cut short the file list of %s", folderPath)
	}
	blobSHAByPath := map[string]string{}
	for _, entry := range tree.Tree {
		if entry.Type == "blob" {
			blobSHAByPath[folderPath+"/"+entry.Path] = entry.SHA
		}
	}
	return conditionalFolderListing{blobSHAByPath: blobSHAByPath, etag: res.Header.Get("ETag")}, nil
}

// Returns the git blob sha of file content `content`, the value GitHub gives as the sha of a file.
// It is the SHA-1 of `blob <size>` and a NUL byte, followed by the content (docs).
// Comparing it with a listed sha tells whether a file already holds `content`, without reading the file.
func computeGitBlobSHA(content []byte) string {
	hash := sha1.New() // `sha1.New`: a hash that takes its input in parts, like a streaming hash object in C++
	fmt.Fprintf(hash, "blob %d%c", len(content), 0)
	hash.Write(content)
	return hex.EncodeToString(hash.Sum(nil))
}

// Reads one file content by its git blob sha (`computeGitBlobSHA`, as a folder listing gives it), not by a path.
// A blob sha names a content, not a place (git docs).
// So the read gives the same bytes wherever the branch is, unlike a read by path and branch name.
//
// Returns:
//   - The content. A `GitHubAPIError` if GitHub answers with a failure (wrapped with `ErrGitHubRateLimited` for a rate limit answer).
//     A sha that GitHub doesn't have is a 404 error, not a missing file
func readDBRepoBlob(ctx context.Context, repo *DBRepo, blobSHA string) ([]byte, error) {
	res, err := CallGitHubAPI(ctx, repo.cfg, repo.BuildAPIPath("/git/blobs/"+url.PathEscape(blobSHA)),
		GitHubAPIRequest{Accept: "application/vnd.github.raw+json"}) // Raw format, so files over 1MB come too (docs)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := CheckGitHubResponseWithRateLimit(res); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(res.Body)
	if err != nil { // The body stopped coming (a dropped connection, `githubTimeout` or the deadline of `ctx`)
		return nil, newGitHubUnreachableError(err)
	}
	return content, nil
}

// The commit at the tip of a branch, and that commit's file tree
type branchHead struct {
	commit string // Commit sha
	tree   string // Tree sha of that commit. Used as the base when making a new tree
}

// Reads the commit sha that the default branch points to.
//
// Returns:
//   - The commit sha. For an empty repository, GitHub refuses with 409 (`GitHubAPIError`, measured)
func readBranchCommit(ctx context.Context, repo *DBRepo) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := callDBRepoAPI(ctx, repo, http.MethodGet, "/git/ref/heads/"+repo.branch, nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

// Builds the part after `/repos/<repository>` of the request that `readBranchHead` sends for `branch`.
// The request lists the commits from that branch, one per page.
func buildBranchTipAPISuffix(branch string) string {
	return "/commits?per_page=1&sha=" + url.QueryEscape("refs/heads/"+branch)
}

// Builds the API path of the request that `readBranchHead` sends for the default branch.
// It is exported so the setup tool can wait for a new branch with the same request that writes start with.
// A 200 answer with a commit in it means that a write can read the branch tip.
func (repo *DBRepo) BuildBranchTipAPIPath() string {
	return repo.BuildAPIPath(buildBranchTipAPISuffix(repo.branch))
}

// Reads the commit that the default branch points to, and its tree, with one request.
//
// NOTE: The list of commits from the branch gives both in its first entry, without the changed files (docs, measured).
// The branch is named in full (`refs/heads/...`), because git looks up a short name as a tag before a branch (git docs).
// The pair comes from GitHub in one answer, so the tree always belongs to the commit.
// A tree from another commit would be the base of the next commit, and that commit would silently undo the files changed since.
func readBranchHead(ctx context.Context, repo *DBRepo) (branchHead, error) {
	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if err := callDBRepoAPI(ctx, repo, http.MethodGet, buildBranchTipAPISuffix(repo.branch), nil, &commits); err != nil {
		return branchHead{}, err
	}
	if len(commits) == 0 || commits[0].SHA == "" || commits[0].Commit.Tree.SHA == "" {
		return branchHead{}, newGitHubUnreadableError(fmt.Errorf("GitHub gave no commit for branch %s", repo.branch))
	}
	return branchHead{commit: commits[0].SHA, tree: commits[0].Commit.Tree.SHA}, nil
}

// Asks GitHub whether a branch contains a commit.
// That is, whether following parents back from the branch tip reaches that commit.
//
// Args:
//   - `branchCommit`: The commit sha that the branch tip points to now
//   - `newCommit`: The commit sha to look for
//
// Returns:
//   - `true` if it contains it (also when it is the same commit)
//
// NOTE: The compare API (`newCommit...branchCommit`) answers "ahead" or "identical" when `branchCommit` contains `newCommit` (docs).
func isCommitInBranch(ctx context.Context, repo *DBRepo, branchCommit string, newCommit string) (bool, error) {
	var compare struct {
		Status string `json:"status"` // ahead, identical, behind, diverged
	}
	if err := callDBRepoAPI(ctx, repo, http.MethodGet, "/compare/"+newCommit+"..."+branchCommit, nil, &compare); err != nil {
		return false, err
	}
	return compare.Status == "ahead" || compare.Status == "identical", nil
}

// Maximum number of tries for one save, the first try included
const maxUpdateAttempts = 5

// Error for a write that has files to write or delete but a commit message that isn't valid (`isValid`).
// Commits that don't say what they changed are never made
var errInvalidCommitMessage = errors.New("refusing to commit changes without a valid commit message")

// Error for a branch that couldn't be moved because another commit landed after the read (returned by `commitFiles`).
// It is a prebuilt error value, so `errors.Is` compares against it (similar to an error code constant in C++).
//
// NOTE: When `UpdateDBRepoFiles` uses up its retries, it returns this error as is, as a 409.
// The signal "try again" and the message for the user ("the archive changed while saving") are the same fact, so they share one value.
var errBranchMoved = Errorf(http.StatusConflict, "The archive changed while saving. Try again")

// Reports whether `p` is a clean path relative to the repository root (e.g. `db/star-lists.json`).
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

// Checks that the commit can be made as it is: it changes something, its message is valid, and its paths are clean.
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
// The Git Data API doesn't work in an empty repository (measured), so the setup tool writes that commit with the Contents API.
//
// Args:
//   - `paths`: Paths of the files to read. Each must pass `isValidRepoPath`
//   - `buildFileChangesCommit`: Takes `filesAtCommit`, the files of `paths` read from one commit.
//     It can read more at the same commit with `filesAtCommit.CommitSHA()` (for example a folder listing, `readBlobSHAsInFolder`).
//     Paths found that way exist in the tree the new commit is built on, so they are safe to put in `FilesToDelete`.
//     It returns the commit to make (`FileChangesCommit`).
//     If nothing is written or deleted, or the changes leave every file as it was, nothing is committed.
//     Otherwise the commit must pass `FileChangesCommit.check`.
//     If it returns an error, nothing is written.
//     It can be called more than once, so it must set any outer variables from scratch on every call
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
//   - One attempt costs "4 + number of files read" GitHub requests (branch tip, one per file read, tree, commit, branch move),
//     plus the requests of `buildFileChangesCommit`, plus one for each written file that isn't UTF-8 text (`commitFiles`).
//     On a retry, a file already read in the request is asked for with its `ETag` (`readDBRepoFileThroughCache`),
//     so an unchanged file is a 304 with no body.
//     This is the price of putting several files in one commit with no in-between state.
func UpdateDBRepoFiles(ctx context.Context, repo *DBRepo, paths []string, buildFileChangesCommit func(filesAtCommit *DBRepoFilesAtCommit) (FileChangesCommit, error)) error {
	for _, p := range paths {
		if !isValidRepoPath(p) {
			return fmt.Errorf("refusing to read a file path that isn't valid: %q", p)
		}
	}
	// Compare-and-swap loop: the commit is built on the branch tip that was read, and the branch moves only if it is still there.
	// If another commit (another device, a sync) got in first, the files are read and the commit is built again on the new tip
	for attempt := 1; ; attempt++ {
		timingStep := beginServerTimingStep(ctx, "w-head")
		head, err := readBranchHead(ctx, repo)
		timingStep.end()
		if err != nil {
			return err
		}
		timingStep = beginServerTimingStep(ctx, "w-read")
		filesAtCommit, err := readDBRepoFilesAtCommit(ctx, repo, paths, head.commit)
		if err != nil {
			timingStep.end()
			return err
		}
		timingStep.endWithDesc(fmt.Sprintf("%s, %d from cache (304)", FormatCount(len(paths), "file"), filesAtCommit.fromCacheCount))
		timingStep = beginServerTimingStep(ctx, "w-build")
		changes, err := buildFileChangesCommit(filesAtCommit)
		timingStep.end()
		if err != nil {
			return err
		}
		if changes.isEmpty() { // Nothing to commit, so the message isn't needed either
			return nil
		}
		if err := changes.check(); err != nil {
			return err
		}

		err = commitFiles(ctx, repo, head, changes)
		if err == nil {
			keepCommittedDBRepoFiles(ctx, changes)
			return nil
		}
		if errors.Is(err, errBranchMoved) && attempt < maxUpdateAttempts { // The only case that is retried is a moved branch
			// Right after another commit, a branch read can briefly point to the old commit (measured).
			// So wait a little longer each time before reading again
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
			continue
		}
		return err
	}
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
//   - Content that isn't UTF-8 costs one more request for each such file, sent one at a time.
//     The server and the setup tool write only UTF-8 (JSON they make, README text from GitHub GraphQL, fixed text).
//     So the path for such content only keeps the promise that any bytes can be written.
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
func commitFiles(ctx context.Context, repo *DBRepo, head branchHead, changes FileChangesCommit) error {
	entries := []map[string]any{} // Tree entries. `any` lets one entry hold a string and another a JSON `null`
	for path, content := range changes.FilesToWrite {
		// The entry carries the content as a JSON string, which holds only UTF-8.
		// A NUL byte is UTF-8 too, and GitHub stores it as it is (measured)
		if utf8.Valid(content) {
			entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "content": string(content)})
			continue
		}
		// Content that isn't UTF-8 can't go in the JSON string.
		// Encoding it as JSON would turn the bad bytes into U+FFFD, so GitHub would store other bytes than ours.
		// The blob API takes the content in base64, which carries any bytes as they are (docs).
		// So the blob is made first with one more request, and the tree entry points to it by its sha instead of carrying the content
		var blob struct {
			SHA string `json:"sha"`
		}
		request := map[string]any{"content": base64.StdEncoding.EncodeToString(content), "encoding": "base64"}
		if err := callDBRepoAPI(ctx, repo, http.MethodPost, "/git/blobs", request, &blob); err != nil {
			return err
		}
		entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "sha": blob.SHA})
	}
	for _, path := range changes.FilesToDelete {
		entries = append(entries, map[string]any{"path": path, "mode": "100644", "type": "blob", "sha": nil}) // `nil` becomes JSON `null`
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	timingStep := beginServerTimingStep(ctx, "w-tree")
	err := callDBRepoAPI(ctx, repo, http.MethodPost, "/git/trees", map[string]any{"base_tree": head.tree, "tree": entries}, &tree)
	timingStep.end()
	if err != nil {
		return err
	}
	if tree.SHA == head.tree { // Every change matched what was already there, so a commit would change nothing
		return nil
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	timingStep = beginServerTimingStep(ctx, "w-commit")
	err = callDBRepoAPI(ctx, repo, http.MethodPost, "/git/commits", map[string]any{"message": changes.Message.String(), "tree": tree.SHA, "parents": []string{head.commit}}, &commit)
	timingStep.end()
	if err != nil {
		return err
	}
	// Moves without force. If the branch moved in the meantime, GitHub refuses
	timingStep = beginServerTimingStep(ctx, "w-move")
	err = callDBRepoAPI(ctx, repo, http.MethodPatch, "/git/refs/heads/"+repo.branch, map[string]any{"sha": commit.SHA, "force": false}, nil)
	timingStep.end()
	if err == nil {
		return nil
	}
	// Doesn't guess the cause from the status code.
	// Reads the branch again and checks whether it contains the new commit
	now, readErr := readBranchCommit(ctx, repo)
	if readErr != nil || now == head.commit { // The branch didn't move: not a conflict
		return err
	}
	included, compareErr := isCommitInBranch(ctx, repo, now, commit.SHA)
	switch {
	case compareErr != nil:
		return err
	case included: // The move happened, only the response was lost
		return nil
	}
	return errBranchMoved // Another commit landed in the meantime
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
//   - `GitHubAPIError` if GitHub answers with a failure.
//     It is wrapped with `ErrGitHubRateLimited` if it is a rate limit answer
func callDBRepoAPI(ctx context.Context, repo *DBRepo, method string, path string, body any, dst any) error {
	res, err := CallGitHubAPI(ctx, repo.cfg, repo.BuildAPIPath(path), GitHubAPIRequest{Method: method, Body: body})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := CheckGitHubResponseWithRateLimit(res); err != nil {
		return err
	}
	if dst == nil {
		return nil
	}
	return DecodeGitHubResponse(res, dst)
}
