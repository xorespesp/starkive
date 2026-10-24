// README snapshots in the archive.
// The start of each archived repository's README is kept in its own file under `ReadmesDBDir`, so it stays after the repository is gone.
// This file covers where the files go (`FetchedReadme.buildSnapshotPath`), finding them (`readReadmeSnapshotsInFolder`),
// what they hold (`buildReadmeSnapshotFile`, `parseReadmeSnapshotFile`), what saving one writes and deletes (`planReadmeSnapshotUpdate`),
// reading a saved one back (`ReadReadmeSnapshot`), and the sync step that keeps them up to date (`SyncReadmeSnapshotsWithGitHub`).
// Picking and fetching READMEs on GitHub is in `github_readmes.go`.
//
// NOTE: These files aren't JSON arrays like the other data files, on purpose.
// One file per repository keeps each change small.
// Like the metadata snapshots, they copy text that the repository owner can change, because keeping what it was is the point.
package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Largest README text kept, in bytes.
const maxReadmeBytes = 16 * 1024

// Line added at the end of a README that was cut at `maxReadmeBytes`.
// It isn't counted in `maxReadmeBytes`.
// It looks like a git merge conflict marker, so it stands out from the README text
const readmeTruncationLine = ">>>>>>> starkive: truncated <<<<<<<"

// End of the file name of the absent marker, which says a repository was checked and its README is absent (`<ID>.no-readme`, empty content).
// It contains `-`, so it never matches a README extension (`readmeExtensionPattern`)
const readmeAbsentMarkerSuffix = ".no-readme"

// Allowed extension of a README snapshot file name.
// A file with any other extension isn't a README snapshot
var readmeExtensionPattern = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// README of one repository as fetched from GitHub: the content to save, or the fact that the README is absent
type FetchedReadme struct {
	isAbsent  bool   // `true` if the repository has no README, or a blank one (`buildReadmeSnapshotFile`). It is saved as the absent marker (`readmeAbsentMarkerSuffix`)
	extension string // Extension of the README file name (`readmeExtensionPattern`). Empty if it has none or it isn't allowed
	content   []byte // File content made by `buildReadmeSnapshotFile`. Empty if `isAbsent`
}

// Builds the path of the file that saves this README for item `id`.
// It is `<ID>.<extension>`, `<ID>` without an extension, or `<ID>.no-readme`, in the folder of `buildReadmeFolderPath`.
func (fetched FetchedReadme) buildSnapshotPath(id int64) string {
	path := fmt.Sprintf("%s/%d", buildReadmeFolderPath(id), id)
	switch {
	case fetched.isAbsent:
		path += readmeAbsentMarkerSuffix
	case fetched.extension != "":
		path += "." + fetched.extension
	}
	return path
}

// Builds the path of the folder that holds the README files of item `id` (`db/readmes/<last two digits of the ID>`)
func buildReadmeFolderPath(id int64) string {
	return fmt.Sprintf("%s/%02d", ReadmesDBDir, id%100)
}

// README snapshot already in the DB repository: one file, as found in a folder listing.
// Its content isn't read.
// `blobSHA` stands for it
type readmeSnapshot struct {
	path     string // Path in the repository
	blobSHA  string // git blob sha of the file content
	isAbsent bool   // `true` if the file is the absent marker (`readmeAbsentMarkerSuffix`)
}

// Collects the README snapshots from a listing of files under `ReadmesDBDir`, by item ID.
// A path that isn't `<ID>`, `<ID>.<extension>` or `<ID>.no-readme` in the folder of that ID is ignored.
//
// Args:
//   - `blobSHAByPath`: Path in the repository -> git blob sha of the file
//
// Returns:
//   - Item ID -> its snapshots, sorted by path
func collectReadmeSnapshotsByID(blobSHAByPath map[string]string) map[int64][]readmeSnapshot {
	snapshotsByID := map[int64][]readmeSnapshot{}
	for path, blobSHA := range blobSHAByPath {
		fileName := path[strings.LastIndexByte(path, '/')+1:]
		idText, extension, hasExtension := strings.Cut(fileName, ".")
		id, err := strconv.ParseInt(idText, 10, 64)
		// Accepts only the exact shape `FetchedReadme.buildSnapshotPath` makes, so a stray file is never deleted or read as a README
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText || path != buildReadmeFolderPath(id)+"/"+fileName {
			continue
		}
		isAbsent := hasExtension && "."+extension == readmeAbsentMarkerSuffix
		if hasExtension && !isAbsent && !readmeExtensionPattern.MatchString(extension) {
			continue
		}
		snapshotsByID[id] = append(snapshotsByID[id], readmeSnapshot{path: path, blobSHA: blobSHA, isAbsent: isAbsent})
	}
	for _, snapshots := range snapshotsByID {
		slices.SortFunc(snapshots, func(a readmeSnapshot, b readmeSnapshot) int { return strings.Compare(a.path, b.path) })
	}
	return snapshotsByID
}

// Reads the README snapshots under folder `folderPath` at commit `commitSHA`, by item ID, with one request.
// The folder is `ReadmesDBDir` or one of its subfolders (`buildReadmeFolderPath`).
// It reads no file content (`readBlobSHAsInFolder`), and keeps only the files `collectReadmeSnapshotsByID` accepts.
func readReadmeSnapshotsInFolder(ctx context.Context, repo *DBRepo, commitSHA string, folderPath string) (map[int64][]readmeSnapshot, error) {
	blobSHAByPath, err := readBlobSHAsInFolder(ctx, repo, commitSHA, folderPath)
	if err != nil {
		return nil, err
	}
	return collectReadmeSnapshotsByID(blobSHAByPath), nil
}

// Reads the paths of every README snapshot of item `itemID` at commit `commitSHA`, with one request (a listing of its folder).
//
// Returns:
//   - The paths, sorted. Empty if the item has no snapshot
func readReadmeSnapshotPaths(ctx context.Context, repo *DBRepo, commitSHA string, itemID int64) ([]string, error) {
	snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, commitSHA, buildReadmeFolderPath(itemID))
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, snapshot := range snapshotsByID[itemID] {
		paths = append(paths, snapshot.path)
	}
	return paths, nil
}

// Adds to `changes` the writes and deletes that bring the saved snapshots of item `itemID` in line with `fetched`.
// `saved` are the snapshots of the item at the commit the change is built on, so each of them is safe to delete.
//   - A README is written unless its file already holds the same content.
//     Every other snapshot of the item (an old extension, the absent marker) is deleted, so the item keeps one file
//   - An absent README keeps a saved README, because keeping what it was is the point.
//     Only an absent marker next to it is deleted
//   - An absent README of an item with no saved README is written as the absent marker
func planReadmeSnapshotUpdate(itemID int64, fetched FetchedReadme, saved []readmeSnapshot, changes *FileChangesCommit) {
	if fetched.isAbsent && slices.ContainsFunc(saved, func(snapshot readmeSnapshot) bool { return !snapshot.isAbsent }) {
		for _, snapshot := range saved {
			if snapshot.isAbsent {
				changes.FilesToDelete = append(changes.FilesToDelete, snapshot.path)
			}
		}
		return
	}
	path := fetched.buildSnapshotPath(itemID)
	isAlreadySaved := false
	for _, snapshot := range saved {
		if snapshot.path == path && snapshot.blobSHA == computeGitBlobSHA(fetched.content) {
			isAlreadySaved = true
		} else if snapshot.path != path {
			changes.FilesToDelete = append(changes.FilesToDelete, snapshot.path)
		}
	}
	if !isAlreadySaved {
		if changes.FilesToWrite == nil {
			changes.FilesToWrite = map[string][]byte{}
		}
		changes.FilesToWrite[path] = fetched.content
	}
}

// Makes the content of a README file from the README text that GitHub gives.
//   - A UTF-8 byte order mark at the start is removed
//   - Text over `maxReadmeBytes` is cut after its last line break in that range,
//     or at a character boundary if there is no line break or it would keep less than half of it.
//     Then `readmeTruncationLine` is added on a line of its own, ending with `\n`
//   - Text that is over `maxReadmeBytes` only because of whitespace at its end loses that whitespace instead of being cut
//
// Returns:
//   - The file content, and `false` if the README is blank
//
// NOTE: GitHub GraphQL gives the text in UTF-8 (measured).
// It doesn't remove a UTF-8 byte order mark, and puts one in front of text it turned from UTF-16 (measured).
func buildReadmeSnapshotFile(readmeText string) ([]byte, bool) {
	text := []byte(strings.TrimPrefix(readmeText, "\uFEFF")) // `\uFEFF` is the byte order mark, written in UTF-8 as `EF BB BF`
	if len(text) > maxReadmeBytes && len(bytes.TrimRight(text, " \t\r\n")) <= maxReadmeBytes {
		text = bytes.TrimRight(text, " \t\r\n") // Only blank space is over the limit, so nothing but blank space is lost
	}
	needsCut := len(text) > maxReadmeBytes
	if needsCut {
		cutAt := maxReadmeBytes
		for i := 1; i < utf8.UTFMax && cutAt > 0 && !utf8.RuneStart(text[cutAt]); i++ { // Moves back to the first byte of the UTF-8 character at `cutAt`, so no character is split
			cutAt--
		}
		end := cutAt
		if lastLineBreak := bytes.LastIndexByte(text[:cutAt], '\n'); lastLineBreak >= 0 {
			end = lastLineBreak + 1
		}
		// A line break near the start (for example, before one long line of HTML) would throw most of the text away
		if end < cutAt/2 {
			end = cutAt
		}
		text = text[:end]
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return nil, false
	}
	if !needsCut {
		return text, true
	}
	if !bytes.HasSuffix(text, []byte("\n")) {
		text = append(text, '\n')
	}
	return append(append(text, readmeTruncationLine...), '\n'), true
}

// Splits the content of a README file into the README text and whether it is truncated (`buildReadmeSnapshotFile` added `readmeTruncationLine`).
//
// NOTE: A README that wasn't cut but itself ends with `readmeTruncationLine` reads as truncated.
// The line names starkive, so no real README is expected to end with it.
func parseReadmeSnapshotFile(content []byte) (string, bool) {
	text, endsWithTruncationLine := bytes.CutSuffix(content, []byte(readmeTruncationLine+"\n"))
	if !endsWithTruncationLine || (len(text) > 0 && !bytes.HasSuffix(text, []byte("\n"))) {
		return string(content), false
	}
	return string(text), true
}

// States of a README snapshot as read back (`ReadmeSnapshotContent.State`)
const (
	readmeStateSaved     = "saved"     // A saved README
	readmeStateAbsent    = "absent"    // Checked, with no README or a blank one (the absent marker)
	readmeStateUnchecked = "unchecked" // Not checked yet (no file)
)

// README snapshot of one item as read back: its state, and the README if one is saved
type ReadmeSnapshotContent struct {
	State     string `json:"state"`               // `saved`, `absent` or `unchecked` (`readmeStateSaved` and so on)
	Format    string `json:"format,omitempty"`    // Extension of the saved file (e.g. `md`). Empty if the file has none
	Text      string `json:"text,omitempty"`      // The saved README text, without `readmeTruncationLine`. Bytes that aren't UTF-8 (only in a file the server didn't write, such as a hand edit) become U+FFFD in JSON
	Truncated bool   `json:"truncated,omitempty"` // `true` if the README was cut at `maxReadmeBytes`
}

// Reads the README snapshot of item `itemID` at the branch tip.
// It reads the commit of the branch tip, lists the item's folder, and reads the file only when a README is saved.
// That is three requests at most.
//
// Returns:
//   - The snapshot. `unchecked` for an item with no file, which includes an ID that isn't in the archive
//   - A GitHub error, or a 502 `HTTPError` if a file in the listing can't be read at the same commit
//
// NOTE: The code leaves one file per item, but a hand edit of the DB repository can leave more.
// A saved README wins over the absent marker, because a saved README is what this feature keeps.
// Among several README files, the first by path is shown.
func ReadReadmeSnapshot(ctx context.Context, repo *DBRepo, itemID int64) (ReadmeSnapshotContent, error) {
	commitSHA, err := readBranchCommit(ctx, repo) // The tree sha isn't needed, so only the commit is read
	if err != nil {
		return ReadmeSnapshotContent{}, err
	}
	snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, commitSHA, buildReadmeFolderPath(itemID))
	if err != nil {
		return ReadmeSnapshotContent{}, err
	}
	snapshots := snapshotsByID[itemID]
	if len(snapshots) == 0 {
		return ReadmeSnapshotContent{State: readmeStateUnchecked}, nil
	}
	for _, snapshot := range snapshots {
		if snapshot.isAbsent {
			continue
		}
		content, err := readDBRepoFileAt(ctx, repo, snapshot.path, commitSHA)
		if err != nil {
			return ReadmeSnapshotContent{}, err
		}
		if content == nil { // The listing at the same commit has the file, so this is GitHub's mistake
			return ReadmeSnapshotContent{}, Errorf(http.StatusBadGateway, "GitHub listed %s but didn't return it", snapshot.path)
		}
		text, isTruncated := parseReadmeSnapshotFile(content)
		_, extension, _ := strings.Cut(snapshot.path[strings.LastIndexByte(snapshot.path, '/')+1:], ".")
		return ReadmeSnapshotContent{State: readmeStateSaved, Format: extension, Text: text, Truncated: isTruncated}, nil
	}
	return ReadmeSnapshotContent{State: readmeStateAbsent}, nil
}

// Number of groups that items with a snapshot are checked in, one group per sync day.
// A sync checks the items whose `ID % readmeRefreshGroups` equals the day number (`syncDayNumber`) modulo it.
// It is a prime, so it shares no factor with the sync intervals (1, 2, 3, 7 days).
// So the groups come up in turn at any interval, and each item with a snapshot is checked again at least once in 61 syncs (61 days when syncing daily)
const readmeRefreshGroups = 61

// Most READMEs written in one commit.
// Making a tree of 2,000 files of 4.7KB (about 9.4MB) took 18.6 seconds (measured).
// So even with every README at `maxReadmeBytes`, one commit stays far below the time limit of one GitHub request (`githubTimeout`)
const maxReadmesPerCommit = 200

// Number of `FetchReadmesByNodeIDs` calls sent at the same time during sync.
// One call for `maxReadmeNodeIDs` repositories took 5.6 to 8.3 seconds (measured).
// 3 at once checked 300 repositories in 29.6 seconds, with no secondary rate limit (measured).
// GitHub allows 60 seconds of GraphQL CPU time per minute (docs), so more at once could reach it
const readmeFetchParallel = 3

// Time kept before `endBy` of `SyncReadmeSnapshotsWithGitHub` for the commit after the last README call of a batch.
// A commit of `maxReadmesPerCommit` READMEs stays far below one request time limit (`githubTimeout`),
// so this leaves room for a slow commit
const readmeCommitReserve = 2 * githubTimeout

// Brings the README snapshots of the archive in line with the READMEs on GitHub, until `endBy`.
//  1. Picks the items to check: items with no snapshot first, then today's group (`readmeRefreshGroups`),
//     then every other item with only the absent marker, each in random order.
//     With `refreshEarly`, the other items with a saved README come last, in random order.
//     Their turn to be refreshed hasn't come yet.
//     Items in `skippedIDs` aren't asked
//  2. Fetches up to `maxReadmesPerCommit` READMEs (`fetchReadmesInParallel`), and writes them in one commit (`writeFetchedReadmesInSingleCommit`)
//  3. Goes on with the next batch while time is left
//
// Args:
//   - `endBy`: Time after which this step fetches no more READMEs.
//     A README call starts only if its first query can end `readmeCommitReserve` before this
//   - `skippedIDs`: IDs of repositories that GitHub can't find now. Asking them would only make absent markers
//   - `now`: Time that picks today's group. Taken as an argument so tests can set the day
//   - `refreshEarly`: Whether to also use the time left to refresh the saved READMEs whose turn to be refreshed hasn't come yet
//
// Returns:
//   - The number of READMEs fetched and compared with the saved ones. Only the changes are committed
//   - An error that needs a person (`isTemporaryGitHubError` is false), from reading the archive, from a commit,
//     or from a batch whose calls all failed (wrapping `errReadmesAllFailed` and the failure of one call).
//     Running out of time, reaching the rate limit, and a GitHub failure that passes on its own aren't errors:
//     what was fetched is committed if time is left for the commit, it is logged, and the next sync goes on
//
// NOTE: Absent markers are asked again in every sync, because a missing record is worse than an old one.
// A README added later, or one that an older rule failed to find, is then saved by the next sync.
// They come after today's group, so they only use the time left.
// However many there are, each item is still checked at least once in `readmeRefreshGroups` syncs.
// The items that `refreshEarly` adds come after them for the same reason.
//
// Snapshots of items no longer in the archive are deleted in every commit it makes.
// With nothing to check, it still runs the commit step for them, and a commit is made only if there is something to delete.
// So files that a deletion outside the server left behind (for example, a hand edit of `db/star-items.json`) go away.
func SyncReadmeSnapshotsWithGitHub(ctx context.Context, repo *DBRepo, endBy time.Time, skippedIDs []int64, now time.Time, refreshEarly bool) (int, error) {
	checked, err := syncReadmeSnapshotBatches(ctx, repo, endBy, skippedIDs, now, refreshEarly)
	if err != nil && isTemporaryGitHubError(ctx, err) {
		log.Printf("readmes: stopped by a GitHub failure that passes on its own, the next sync goes on: %v", err)
		return checked, nil
	}
	return checked, err
}

// Does the work of `SyncReadmeSnapshotsWithGitHub`, with the same arguments.
// It returns every error, including the ones that pass on their own
func syncReadmeSnapshotBatches(ctx context.Context, repo *DBRepo, endBy time.Time, skippedIDs []int64, now time.Time, refreshEarly bool) (int, error) {
	commitSHA, err := readBranchCommit(ctx, repo)
	if err != nil {
		return 0, err
	}
	starItems, err := starItemsDataFile.readLatestFromDBRepo(ctx, repo)
	if err != nil {
		return 0, err
	}
	snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, commitSHA, ReadmesDBDir)
	if err != nil {
		return 0, err
	}
	isSkipped := map[int64]bool{}
	for _, id := range skippedIDs {
		isSkipped[id] = true
	}
	todaysGroup := syncDayNumber(now) % readmeRefreshGroups
	uncheckedItems := []StarItem{}
	todaysItems := []StarItem{}
	absentItems := []StarItem{}
	otherItems := []StarItem{} // Items with a saved README outside today's group. Checked only with `refreshEarly`
	for _, item := range starItems {
		snapshots := snapshotsByID[item.ID]
		switch {
		case isSkipped[item.ID]:
		case item.NodeID == "": // An item without `node_id` can't be asked about with GraphQL
		case len(snapshots) == 0:
			uncheckedItems = append(uncheckedItems, item)
		case item.ID%readmeRefreshGroups == todaysGroup:
			todaysItems = append(todaysItems, item)
		case !slices.ContainsFunc(snapshots, func(snapshot readmeSnapshot) bool { return !snapshot.isAbsent }): // No saved README
			absentItems = append(absentItems, item)
		default:
			otherItems = append(otherItems, item)
		}
	}
	// Shuffled, so a run that can't check them all leaves different items for the next run
	rand.Shuffle(len(uncheckedItems), func(i int, j int) { uncheckedItems[i], uncheckedItems[j] = uncheckedItems[j], uncheckedItems[i] })
	rand.Shuffle(len(todaysItems), func(i int, j int) { todaysItems[i], todaysItems[j] = todaysItems[j], todaysItems[i] })
	rand.Shuffle(len(absentItems), func(i int, j int) { absentItems[i], absentItems[j] = absentItems[j], absentItems[i] })
	queue := slices.Concat(uncheckedItems, todaysItems, absentItems)
	if refreshEarly {
		rand.Shuffle(len(otherItems), func(i int, j int) { otherItems[i], otherItems[j] = otherItems[j], otherItems[i] })
		queue = append(queue, otherItems...)
	}

	fetchEndBy := endBy.Add(-readmeCommitReserve) // Time by which the README calls of a batch end, so the commit has time left
	checked := 0
	for {
		batch := queue[:min(maxReadmesPerCommit, len(queue))] // Can be empty: the commit still deletes the snapshots of removed items
		fetched, stopErr := fetchReadmesInParallel(ctx, repo.cfg, batch, fetchEndBy)
		// A call can end right at `fetchEndBy`, so the time left can be a little under `readmeCommitReserve`.
		// Half of it is still enough for the commit, and the READMEs fetched aren't thrown away
		if time.Until(endBy) < readmeCommitReserve/2 {
			log.Printf("readmes: no time left to commit %d fetched READMEs", len(fetched))
			return checked, nil
		}
		if err := writeFetchedReadmesInSingleCommit(ctx, repo, fetched); err != nil {
			return checked, err
		}
		checked += len(fetched)
		queue = queue[len(batch):]
		if errors.Is(stopErr, errReadmesAllFailed) {
			return checked, stopErr
		}
		if stopErr != nil {
			log.Printf("readmes: stopped with %d READMEs left for the next sync: %v", len(queue)+len(batch)-len(fetched), stopErr)
			return checked, nil
		}
		if len(queue) == 0 {
			return checked, nil
		}
	}
}

// Error for README calls that weren't sent, or were cut, because the time ran out
var errReadmesOutOfTime = errors.New("out of time")

// Error for a batch whose README calls all failed.
// GitHub is likely failing as a whole, or the setup is broken, so asking more would only use up the rate limit.
// It is wrapped together with the failure of one call, which tells whether it passes on its own (`isTemporaryGitHubError`)
var errReadmesAllFailed = errors.New("every README call of a batch failed")

// Fetches the READMEs of the items `items`, `maxReadmeNodeIDs` in each `FetchReadmesByNodeIDs` call and `readmeFetchParallel` calls at a time.
// A call starts only if its first query can end by `fetchEndBy` even when it waits the longest (`httpClient.Timeout`, that is `githubTimeout`).
// A call still going at `fetchEndBy` is cut there.
//
// Returns:
//   - The READMEs fetched: item ID -> README.
//     An item that GitHub can't tell about now, or whose call failed, isn't in it, and is asked again later
//   - Why not every call was answered: `errReadmesOutOfTime` (not sent in time, or cut at `fetchEndBy`),
//     or an error that wraps `errGitHubRateLimited`.
//     If every call failed, an error that wraps `errReadmesAllFailed` and the failure of one call, so no more batches are asked.
//     A failure that needs a person is picked over one that passes on its own.
//     `nil` if every call ended and at least one succeeded
//
// NOTE: A call usually has up to three queries in a row, each ending in a few seconds as a rule (measured).
// One cut at `fetchEndBy` counts as running out of time, not as a failure of GitHub, and its items are asked again later.
// A failure about one repository isn't a failed call: GraphQL answers the others, and that one isn't fetched.
func fetchReadmesInParallel(ctx context.Context, cfg *EnvConfig, items []StarItem, fetchEndBy time.Time) (map[int64]FetchedReadme, error) {
	// `WithDeadline` makes a `ctx` with an earlier deadline, `fetchEndBy` (never later than the deadline of `ctx`).
	// `cancel` frees its timer once the calls are done
	fetchCtx, cancel := context.WithDeadline(ctx, fetchEndBy)
	defer cancel()
	groups := [][]StarItem{} // `items` split into the items of each call
	for start := 0; start < len(items); start += maxReadmeNodeIDs {
		groups = append(groups, items[start:min(start+maxReadmeNodeIDs, len(items))])
	}
	// Several goroutines (lightweight threads) take groups one by one.
	// Shared values are read and written while holding `mu`
	var mu sync.Mutex
	next := 0
	fetched := map[int64]FetchedReadme{}
	failed := 0
	succeeded := 0
	var failure error // The failure of one call, kept for `errReadmesAllFailed`
	var stopErr error
	var wg sync.WaitGroup // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < readmeFetchParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if stopErr == nil && next < len(groups) && time.Now().Add(httpClient.Timeout).After(fetchEndBy) {
					stopErr = errReadmesOutOfTime
				}
				if stopErr != nil || next >= len(groups) {
					mu.Unlock()
					return
				}
				group := groups[next]
				next++
				mu.Unlock()

				nodeIDs := make([]string, len(group))
				for i, item := range group {
					nodeIDs[i] = item.NodeID
				}
				readmes, err := FetchReadmesByNodeIDs(fetchCtx, cfg, nodeIDs)
				mu.Lock()
				switch {
				case errors.Is(err, errGitHubRateLimited):
					if stopErr == nil {
						stopErr = err
					}
				case err != nil && fetchCtx.Err() != nil:
					// Cut at the deadline: the time ran out, which says nothing about GitHub.
					// It is told by `fetchCtx`, because a single slow request (`githubTimeout`) gives the same error
					if stopErr == nil {
						stopErr = errReadmesOutOfTime
					}
				case err != nil:
					failed++
					if failure == nil || isTemporaryGitHubError(ctx, failure) {
						failure = err
					}
				default:
					succeeded++
					for i, readme := range readmes {
						if readme != nil {
							fetched[group[i].ID] = *readme
						}
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed > 0 {
		log.Printf("readmes: %d of %d calls failed, their items are asked again later", failed, failed+succeeded)
	}
	if stopErr == nil && failed > 0 && succeeded == 0 {
		stopErr = fmt.Errorf("%w: %w", errReadmesAllFailed, failure) // Two `%w`: `errors.Is` and `errors.As` find both errors
	}
	return fetched, stopErr
}

// Writes the READMEs `fetched` in one commit, and deletes in it the snapshots of items no longer in the archive.
// It reads the archive and lists `ReadmesDBDir` at the commit it builds on, so items removed meanwhile are skipped.
// Nothing is committed if nothing changes.
func writeFetchedReadmesInSingleCommit(ctx context.Context, repo *DBRepo, fetched map[int64]FetchedReadme) error {
	return UpdateDBRepoFiles(ctx, repo, []string{starItemsDataFile.path}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
		starItems, err := starItemsDataFile.decodeForReading(filesRead)
		if err != nil {
			return FileChangesCommit{}, err
		}
		isArchived := make(map[int64]bool, len(starItems)) // Item ID -> `true`, for the items in the archive
		for _, item := range starItems {
			isArchived[item.ID] = true
		}
		snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, repo, commitSHA, ReadmesDBDir)
		if err != nil {
			return FileChangesCommit{}, err
		}

		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}}
		changedCount := 0             // Number of items whose files change, including ones that only lose an old file
		savedNames := []string{}      // Items whose README is written
		absentNames := []string{}     // Items that get the absent marker
		isPlanned := map[int64]bool{} // Item ID -> `true` once planned, so an ID listed twice in the archive is planned once
		for _, item := range starItems {
			readme, ok := fetched[item.ID]
			if !ok || isPlanned[item.ID] {
				continue
			}
			isPlanned[item.ID] = true
			changesBefore := len(changes.FilesToWrite) + len(changes.FilesToDelete)
			planReadmeSnapshotUpdate(item.ID, readme, snapshotsByID[item.ID], &changes)
			if len(changes.FilesToWrite)+len(changes.FilesToDelete) > changesBefore {
				changedCount++
			}
			if _, isWritten := changes.FilesToWrite[readme.buildSnapshotPath(item.ID)]; isWritten {
				if readme.isAbsent {
					absentNames = append(absentNames, item.Name)
				} else {
					savedNames = append(savedNames, item.Name)
				}
			}
		}
		removedCount := 0 // Number of items no longer in the archive whose snapshots are deleted
		for id, snapshots := range snapshotsByID {
			// A missing item file reads as an empty archive, and would delete every snapshot.
			// It may be a passing 404 for a file that is there, so nothing is deleted then
			if isArchived[id] || filesRead[StarItemsDBPath] == nil {
				continue
			}
			removedCount++
			for _, snapshot := range snapshots {
				changes.FilesToDelete = append(changes.FilesToDelete, snapshot.path)
			}
		}

		summary := "update readmes of " + FormatCount(changedCount, "repo")
		removedLine := ""
		switch {
		case changedCount == 0:
			summary = "remove readmes of " + FormatCount(removedCount, "repo") + " no longer in the archive"
		case removedCount > 0:
			removedLine = "Removed for " + FormatCount(removedCount, "repo") + " no longer in the archive"
		}
		changes.Message = NewCommitMessage("sync(readmes)", summary, formatCommitList("Saved", savedNames), formatCommitList("No README", absentNames), removedLine)
		return changes, nil
	})
}
