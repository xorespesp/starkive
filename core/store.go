// Storage engine for files in the DB repository, and the archive data.
// It covers reading a file (`ReadFile`), writing several files in one commit (`UpdateFiles`, Git Data API),
// handling the three archive files together (`UpdateArchive`), commit messages (`CommitMessage`),
// and the data file format (`encodeLines`).
// `db_repo.go` decides which repository to use, and whether it is safe to write to.
package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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
	StarItems []StarItem // Archive entries (`StarItemsDBPath`)
	StarLists []StarList // Star list definitions (`StarListsDBPath`)
	RepoMeta  []RepoMeta // Snapshots of repository metadata (`RepoMetaDBPath`)
}

// Error for a write that has files to write but an empty commit message.
// Commits that don't say what they changed are never made
var errEmptyCommitMessage = errors.New("refusing to commit changes without a commit message")

// Reads several files in the DB repository from one commit, and writes the files that `change` returns in one commit.
// Every write to the DB repository goes through this function
// (the archive through `UpdateArchive`, the config file through `updateConfig`, the workflow through the setup tool).
// The only exception is the first commit in an empty repository.
// The Git Data API doesn't work in an empty repository, so the setup tool writes that commit with the Contents API.
//
// Args:
//   - `paths`: Paths of the files to read
//   - `change`: Takes what was read (path -> content, `nil` if the file doesn't exist).
//     It returns the files to write (path -> new content) and a commit message.
//     If there are no files to write, nothing is committed.
//     If there are files to write, the commit message is required.
//     If it returns an error, nothing is written
//
// Returns:
//   - An error from `change` or from GitHub.
//     `errEmptyCommitMessage` if there are files to write but the message is empty.
//     409 `errBranchMoved` if other commits keep getting in until all retries are used up
//
// NOTE:
//   - Even when several files change, there is only one commit.
//     So a change like "delete a star list and clean up its members" has no in-between state.
//   - If another commit (another device, a sync) lands after the read, GitHub refuses to move the branch (`errBranchMoved`).
//     Then it starts over from the new commit.
//     This is the only case that is retried.
//     Errors while making the tree or the commit are not conflicts, so they are returned right away.
//   - `change` can be called more than once.
//     If it leaves results in outer variables, it must set them from scratch on every call.
//   - One write costs "5 + number of files" GitHub requests (branch, its commit, one per file, tree, commit, branch move).
//     For the archive (three files) that is 8.
//     This is the price of putting several files in one commit with no in-between state.
func UpdateFiles(cfg *Config, paths []string, change func(original map[string][]byte) (map[string][]byte, CommitMessage, error)) error {
	for attempt := 1; ; attempt++ {
		head, err := readBranchHead(cfg)
		if err != nil {
			return err
		}
		original, err := readFilesAt(cfg, paths, head.commit)
		if err != nil {
			return err
		}
		written, message, err := change(original) // If another commit gets in, it is called again on the next loop with the new content
		if err != nil {
			return err
		}
		if len(written) == 0 {
			return nil
		}
		if message.subject == "" { // Commits that don't say what they changed are never made
			return errEmptyCommitMessage
		}

		err = commitFiles(cfg, head, written, message)
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
	err := UpdateFiles(cfg, []string{StarItemsDBPath, StarListsDBPath, RepoMetaDBPath}, func(original map[string][]byte) (map[string][]byte, CommitMessage, error) {
		a := &Archive{}
		var err error
		if a.StarItems, err = decodeItems[StarItem](original[StarItemsDBPath]); err != nil {
			return nil, CommitMessage{}, err
		}
		if a.StarLists, err = decodeItems[StarList](original[StarListsDBPath]); err != nil {
			return nil, CommitMessage{}, err
		}
		if a.RepoMeta, err = decodeItems[RepoMeta](original[RepoMetaDBPath]); err != nil {
			return nil, CommitMessage{}, err
		}
		before, err := encode(a) // `change` edits `a` in place, so encode it first
		if err != nil {
			return nil, CommitMessage{}, err
		}

		message, err := change(a)
		if err != nil {
			return nil, CommitMessage{}, err
		}

		after, err := encode(a)
		if err != nil {
			return nil, CommitMessage{}, err
		}
		written := map[string][]byte{}
		for path, content := range after {
			if !bytes.Equal(before[path], content) {
				written[path] = content
			}
		}
		updated = a // If called again, this becomes the new archive
		return written, message, nil
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

// Writes several files in one commit (GitHub Git Data API):
//  1. Makes a new tree from the tree of `head`, with only the changed files replaced
//  2. Makes a new commit with `head` as its parent
//  3. Moves the branch to the new commit (without force)
//
// Args:
//   - `head`: The branch tip at the time the files were read. It becomes the parent of the new commit
//   - `files`: Files to write (path -> new content)
//   - `message`: The commit message
//
// Returns:
//   - `nil` on success. `errBranchMoved` if the branch moved in the meantime. Otherwise the GitHub error
//
// NOTE:
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
func commitFiles(cfg *Config, head branchHead, files map[string][]byte, message CommitMessage) error {
	entries := []map[string]string{}
	for path, content := range files {
		entries = append(entries, map[string]string{"path": path, "mode": "100644", "type": "blob", "content": string(content)})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if err := callRepoAPI(cfg, http.MethodPost, "/git/trees", map[string]any{"base_tree": head.tree, "tree": entries}, &tree); err != nil {
		return err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := callRepoAPI(cfg, http.MethodPost, "/git/commits", map[string]any{"message": message.String(), "tree": tree.SHA, "parents": []string{head.commit}}, &commit); err != nil {
		return err
	}
	// Moves without force. If the branch moved in the meantime, GitHub refuses
	err := callRepoAPI(cfg, http.MethodPatch, "/git/refs/heads/"+cfg.DBRepoBranch, map[string]any{"sha": commit.SHA, "force": false}, nil)
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
	subject string   // Subject in the form `<who>(<file>): <summary>`
	body    []string // Body blocks, added below the subject with blank lines between them
}

// Builds a commit message.
// The subject is `<prefix>: <summary>`, so commits can be filtered by who made them, like `git log --grep "^sync("`.
//
// Args:
//   - `prefix`: Who made the change and which file changed, written like `user(star-items)`.
//     Who is `user` (something a person did in the dashboard or the setup tool) or `sync` (auto sync).
//     The file is `star-items`, `star-lists`, `repo-meta`, `config`, or `workflow` (the workflow and README)
//   - `summary`: What changed, starting with a verb (e.g. "add owner/repo")
//   - `body`: Body blocks (use `formatCommitList` for a list of many items). Empty blocks are skipped
//
// NOTE: The fields are private, so other packages can only make messages with this function.
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
