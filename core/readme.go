// README snapshots.
// The start of each archived repository's README is kept in its own file under `ReadmesDBDir`, so it stays after the repository is gone.
// This file covers where the files go (`FetchedReadme.buildPath`), finding them (`readReadmeSnapshotsInFolder`),
// what they hold (`buildReadmeSnapshotFile`, `parseReadmeSnapshotFile`), what saving one writes and deletes (`planReadmeSnapshotUpdate`),
// fetching a README from GitHub (`FetchReadmeByRepoID`), reading a saved one back (`ReadReadmeSnapshot`),
// and the sync step that keeps them up to date (`SyncReadmeSnapshotsWithGitHub`).
//
// NOTE: These files aren't JSON arrays like the other data files, on purpose.
// One file per repository keeps each change small.
// Like the metadata snapshots, they copy text that the repository owner can change, because keeping what it was is the point.
package core

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Folder of the README snapshots.
// The files of a repository go in the subfolder named by the last two digits of its ID (`buildReadmeFolderPath`).
// The last two digits of real IDs spread evenly (measured), so a folder holds about 200 files at 20,000 repositories.
const ReadmesDBDir = "db/readmes"

// Largest README text kept, in bytes.
// It holds the whole README for about 70% of repositories (measured on 729 READMEs, median 4.3KB).
const maxReadmeBytes = 8 * 1024

// Line added at the end of a README that was cut at `maxReadmeBytes`.
// It isn't counted in `maxReadmeBytes`.
// It looks like a git merge conflict marker, so it stands out from the README text
const readmeCutLine = ">>>>>>> starkive: truncated <<<<<<<"

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
func (fetched FetchedReadme) buildPath(id int64) string {
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
// Its content isn't read. `blobSHA` stands for it
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
		// Accepts only the exact shape `FetchedReadme.buildPath` makes, so a stray file is never deleted or read as a README
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
func readReadmeSnapshotsInFolder(cfg *Config, commitSHA string, folderPath string) (map[int64][]readmeSnapshot, error) {
	blobSHAByPath, err := readBlobSHAsInFolder(cfg, commitSHA, folderPath)
	if err != nil {
		return nil, err
	}
	return collectReadmeSnapshotsByID(blobSHAByPath), nil
}

// Reads the paths of every README snapshot of item `itemID` at commit `commitSHA`, with one request (a listing of its folder).
//
// Returns:
//   - The paths, sorted. Empty if the item has no snapshot
func readReadmeSnapshotPaths(cfg *Config, commitSHA string, itemID int64) ([]string, error) {
	snapshotsByID, err := readReadmeSnapshotsInFolder(cfg, commitSHA, buildReadmeFolderPath(itemID))
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
	path := fetched.buildPath(itemID)
	isAlreadySaved := false
	for _, snapshot := range saved {
		if snapshot.path == path && snapshot.blobSHA == gitBlobSHA(fetched.content) {
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

// Makes the content of a README file from the README bytes as GitHub stores them.
//   - Text with a UTF-16 byte order mark is turned into UTF-8. A UTF-8 byte order mark is removed.
//     Other text is kept as bytes, even when it isn't UTF-8 (an older encoding such as EUC-KR)
//   - Text over `maxReadmeBytes` is cut after its last line break in that range,
//     or at a UTF-8 character boundary if that line break would keep less than half of it.
//     Then `readmeCutLine` is added on a line of its own, ending with `\n`
//   - Text that is over `maxReadmeBytes` only because of whitespace at its end loses that whitespace instead of being cut
//
// Args:
//   - `sourceBytes`: The README as GitHub stores it.
//     It can be only the start of the file, if that start is longer than `maxReadmeBytes` after it is turned into UTF-8
//
// Returns:
//   - The file content, and `false` if the README is blank
//
// NOTE: Text that isn't UTF-8 is kept, because keeping the README is the point and its encoding can be guessed when it is shown.
// Common older encodings (EUC-KR, Shift_JIS, GBK, Big5) never use the line break byte inside a character,
// so a cut after a line break keeps every character whole.
// UTF-16 without a byte order mark keeps the NUL byte after the line break, so its characters stay whole too.
// Only a cut with no line break near can split the last character of such text.
// `readmeCutLine` is ASCII, so in UTF-16 text it doesn't read as the same characters.
// `parseReadmeSnapshotFile` still finds it, because it compares bytes.
func buildReadmeSnapshotFile(sourceBytes []byte) ([]byte, bool) {
	var text []byte
	switch {
	case bytes.HasPrefix(sourceBytes, []byte{0xFF, 0xFE}):
		text = decodeUTF16(sourceBytes[2:], binary.LittleEndian)
	case bytes.HasPrefix(sourceBytes, []byte{0xFE, 0xFF}):
		text = decodeUTF16(sourceBytes[2:], binary.BigEndian)
	default:
		text = bytes.TrimPrefix(sourceBytes, []byte("\xEF\xBB\xBF"))
	}
	if len(text) > maxReadmeBytes && len(bytes.TrimRight(text, " \t\r\n")) <= maxReadmeBytes {
		text = bytes.TrimRight(text, " \t\r\n") // Only blank space is over the limit, so nothing but blank space is lost
	}
	isCut := len(text) > maxReadmeBytes
	if isCut {
		cutAt := maxReadmeBytes
		for i := 1; i < utf8.UTFMax && cutAt > 0 && !utf8.RuneStart(text[cutAt]); i++ { // Moves back to the first byte of the UTF-8 character at `cutAt`, so no UTF-8 character is split
			cutAt--
		}
		// The line break is looked for one byte before `cutAt`, so a NUL byte after it still fits.
		// In UTF-16 without a byte order mark, a line break is `\n` followed by NUL, and both must stay together
		end := cutAt
		if lastLineBreak := bytes.LastIndexByte(text[:cutAt-1], '\n'); lastLineBreak >= 0 {
			end = lastLineBreak + 1
			if text[end] == 0 {
				end++
			}
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
	if !isCut {
		return text, true
	}
	content := append([]byte{}, text...) // Copies, so appending never writes into `sourceBytes`
	if !bytes.HasSuffix(content, []byte("\n")) {
		content = append(content, '\n')
	}
	return append(append(content, readmeCutLine...), '\n'), true
}

// Turns `utf16Bytes` in byte order `order` into UTF-8. A last odd byte is dropped
func decodeUTF16(utf16Bytes []byte, order binary.ByteOrder) []byte {
	codeUnits := make([]uint16, len(utf16Bytes)/2)
	for i := range codeUnits {
		codeUnits[i] = order.Uint16(utf16Bytes[2*i:])
	}
	return []byte(string(utf16.Decode(codeUnits)))
}

// Splits the content of a README file into the README text and whether it was cut (`buildReadmeSnapshotFile` added `readmeCutLine`).
//
// NOTE: A README that isn't cut but itself ends with `readmeCutLine` reads as cut.
// The line names starkive, so no real README is expected to end with it.
func parseReadmeSnapshotFile(content []byte) (string, bool) {
	text, endsWithCutLine := bytes.CutSuffix(content, []byte(readmeCutLine+"\n"))
	if !endsWithCutLine || (len(text) > 0 && !bytes.HasSuffix(text, []byte("\n"))) {
		return string(content), false
	}
	return string(text), true
}

// Error for a GitHub answer that says the rate limit was reached
var errGitHubRateLimited = errors.New("GitHub rate limit reached")

// Checks a GitHub answer like `CheckGitHubAPI`, and wraps a rate limit answer with `errGitHubRateLimited`.
// A rate limit answer is 429, or 403 with no requests left, a `Retry-After` header, or "rate limit" in the message (docs).
func checkGitHubAPIWithRateLimit(res *http.Response) error {
	err := CheckGitHubAPI(res)
	var apiErr *GitHubAPIError
	if !errors.As(err, &apiErr) {
		return err
	}
	isRateLimited := res.StatusCode == http.StatusTooManyRequests ||
		(res.StatusCode == http.StatusForbidden && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(apiErr.Message), "rate limit")))
	if isRateLimited {
		return fmt.Errorf("%w: %w", errGitHubRateLimited, err) // Two `%w`: `errors.Is` finds both errors
	}
	return err
}

// Error for a request that isn't sent because its deadline has passed
var errDeadlinePassed = errors.New("deadline passed before the request")

// Returns the time left until `deadline`, as the time limit of the next request.
// It returns `errDeadlinePassed` if no time is left.
// A zero `GitHubAPIRequest.Timeout` means the default limit, so a request must never get one in place of "no time left".
func timeLeftUntil(deadline time.Time) (time.Duration, error) {
	timeLeft := time.Until(deadline)
	if timeLeft <= 0 {
		return 0, errDeadlinePassed
	}
	return timeLeft, nil
}

// Fetches the README of repository `id` from GitHub, with all its requests ending by `deadline`.
// A README over 1MB needs two requests, and both share the same deadline.
//
// Returns:
//   - The README. It is absent (`isAbsent`) when GitHub answers the first request with 404 or 451,
//     or when the README is blank (`buildReadmeSnapshotFile`)
//   - An error that wraps `errGitHubRateLimited` if GitHub says the rate limit was reached.
//     Any other error (a 403, a timeout, 5xx, `errDeadlinePassed`) means "ask again later", and says nothing about the README
//
// NOTE: Only 404 (no README) and 451 (blocked for legal reasons) say the README is absent (docs).
// A 403 says this token can't see it now, not that there is no README.
// So a 403 is an error like any other failure, and the README is asked again later.
// It asks by ID (`/repositories/{id}/readme`), so a renamed repository needs no lookup (measured).
// The default answer has the file name and the content in base64.
// For a file over 1MB the content is empty (docs), so the start of the file is read again in raw format.
func FetchReadmeByRepoID(cfg *Config, id int64, deadline time.Time) (FetchedReadme, error) {
	timeLeft, err := timeLeftUntil(deadline)
	if err != nil {
		return FetchedReadme{}, err
	}
	path := fmt.Sprintf("/repositories/%d/readme", id)
	res, err := CallGitHubAPI(cfg, path, GitHubAPIRequest{Timeout: timeLeft})
	if err != nil {
		return FetchedReadme{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusUnavailableForLegalReasons {
		return FetchedReadme{isAbsent: true}, nil
	}
	if err := checkGitHubAPIWithRateLimit(res); err != nil {
		return FetchedReadme{}, err
	}
	var readmeFile struct {
		Name    string `json:"name"`    // File name (e.g. `README.md`)
		Size    int64  `json:"size"`    // File size in bytes
		Content string `json:"content"` // Content in base64, broken into lines. Empty for a file over 1MB
	}
	if err := json.NewDecoder(res.Body).Decode(&readmeFile); err != nil {
		return FetchedReadme{}, err
	}
	var sourceBytes []byte
	if readmeFile.Content == "" && readmeFile.Size > 0 {
		// UTF-16 text takes up to twice the bytes of the same text in UTF-8.
		// Four times `maxReadmeBytes` still keeps more than `maxReadmeBytes` after it is turned into UTF-8,
		// so `buildReadmeSnapshotFile` cuts it and adds `readmeCutLine`
		if sourceBytes, err = fetchRawReadme(cfg, path, deadline, 4*maxReadmeBytes); err != nil {
			return FetchedReadme{}, err
		}
	} else if sourceBytes, err = base64.StdEncoding.DecodeString(readmeFile.Content); err != nil { // It skips the line breaks GitHub puts in (docs)
		return FetchedReadme{}, err
	}
	content, isUsable := buildReadmeSnapshotFile(sourceBytes)
	if !isUsable {
		return FetchedReadme{isAbsent: true}, nil
	}
	extension := ""
	if dot := strings.LastIndexByte(readmeFile.Name, '.'); dot >= 0 {
		extension = strings.ToLower(readmeFile.Name[dot+1:])
	}
	if !readmeExtensionPattern.MatchString(extension) {
		extension = "" // An extension that isn't allowed is dropped, and the file is saved as `<ID>`
	}
	return FetchedReadme{extension: extension, content: content}, nil
}

// Fetches the README at API path `path` in raw format, up to `maxBytes` from its start, ending by `deadline`.
// Reading stops there and the answer is closed, so most of a large README isn't downloaded.
func fetchRawReadme(cfg *Config, path string, deadline time.Time, maxBytes int64) ([]byte, error) {
	timeLeft, err := timeLeftUntil(deadline)
	if err != nil {
		return nil, err
	}
	res, err := CallGitHubAPI(cfg, path, GitHubAPIRequest{Accept: "application/vnd.github.raw+json", Timeout: timeLeft})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := checkGitHubAPIWithRateLimit(res); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(res.Body, maxBytes)) // `io.LimitReader`: stops reading after that many bytes
}

// States of a README snapshot as read back (`ReadmeSnapshotContent.State`)
const (
	readmeStateSaved     = "saved"     // A saved README
	readmeStateAbsent    = "absent"    // Checked, with no README or a blank one (the absent marker)
	readmeStateUnchecked = "unchecked" // Not checked yet (no file)
)

// README snapshot of one item as read back: its state, and the README if one is saved
type ReadmeSnapshotContent struct {
	State     string `json:"state"`                // `saved`, `absent` or `unchecked` (`readmeStateSaved` and so on)
	Format    string `json:"format,omitempty"`     // Extension of the saved file (e.g. `md`). Empty if the file has none
	Text      string `json:"text,omitempty"`       // The saved README text, without `readmeCutLine`. Empty if it isn't UTF-8 text
	RawBase64 string `json:"raw_base64,omitempty"` // The saved README in base64, in place of `Text` when it isn't UTF-8 text. Its encoding isn't detected
	Truncated bool   `json:"truncated,omitempty"`  // `true` if the README was cut at `maxReadmeBytes`
}

// Reads the README snapshot of item `itemID` at the branch tip.
// It reads the commit of the branch tip, lists the item's folder, and reads the file only when a README is saved.
// That is three requests at most.
//
// Returns:
//   - The snapshot. `unchecked` for an item with no file, which includes an ID that isn't in the archive.
//     A GitHub error, or a 502 `HTTPError` if a file in the listing can't be read at the same commit
//
// NOTE: The code leaves one file per item, but a hand edit of the DB repository can leave more.
// A saved README wins over the absent marker, because a saved README is what this feature keeps.
// Among several README files, the first by path is shown.
func ReadReadmeSnapshot(cfg *Config, itemID int64) (ReadmeSnapshotContent, error) {
	commitSHA, err := readBranchCommit(cfg) // The tree sha isn't needed, so only the commit is read
	if err != nil {
		return ReadmeSnapshotContent{}, err
	}
	snapshotsByID, err := readReadmeSnapshotsInFolder(cfg, commitSHA, buildReadmeFolderPath(itemID))
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
		content, err := readFileAt(cfg, snapshot.path, commitSHA)
		if err != nil {
			return ReadmeSnapshotContent{}, err
		}
		if content == nil { // The listing at the same commit has the file, so this is GitHub's mistake
			return ReadmeSnapshotContent{}, Errorf(http.StatusBadGateway, "GitHub listed %s but didn't return it", snapshot.path)
		}
		text, isTruncated := parseReadmeSnapshotFile(content)
		_, extension, _ := strings.Cut(snapshot.path[strings.LastIndexByte(snapshot.path, '/')+1:], ".")
		saved := ReadmeSnapshotContent{State: readmeStateSaved, Format: extension, Truncated: isTruncated}
		// A NUL byte means UTF-16 without a byte order mark (or binary), which isn't UTF-8 text either
		if utf8.ValidString(text) && !strings.ContainsRune(text, 0) {
			saved.Text = text
		} else {
			saved.RawBase64 = base64.StdEncoding.EncodeToString([]byte(text)) // JSON strings hold only UTF-8, so the bytes go as base64
		}
		return saved, nil
	}
	return ReadmeSnapshotContent{State: readmeStateAbsent}, nil
}

// Number of groups that items with a snapshot are checked in, one group per sync day.
// A sync checks the items whose `ID % readmeRefreshGroups` equals the day number (`syncDayNumber`) modulo it.
// It is a prime, so it shares no factor with the sync intervals (1, 2, 3, 7 days).
// So the groups come up in turn at any interval, and each item with a snapshot is checked again once in 61 syncs (61 days when syncing daily)
const readmeRefreshGroups = 61

// Most READMEs written in one commit.
// Making a tree of 300 files of 4.7KB took about 4 seconds, and 2,000 files took 18.6 seconds (measured).
// This keeps one commit far below the request time limit (`githubTimeout`)
const maxReadmesPerCommit = 300

// Number of README requests sent at the same time during sync.
// One takes about 0.5 seconds (measured), so 6 at once check 300 in about 25 seconds
const readmeFetchParallel = 6

// Time kept before the deadline for the commit after the last README request of a batch.
// A commit of `maxReadmesPerCommit` READMEs makes a tree that took about 4 seconds (measured),
// so this leaves room for a slow commit
const readmeCommitReserve = 2 * githubTimeout

// Brings the README snapshots of the archive in line with the READMEs on GitHub, until `deadline`.
//  1. Picks the items to check: items with no snapshot first, then today's group (`readmeRefreshGroups`), each in random order.
//     Items in `skippedIDs` aren't asked
//  2. Fetches up to `maxReadmesPerCommit` READMEs, `readmeFetchParallel` at a time, and writes them in one commit (`writeFetchedReadmesInSingleCommit`)
//  3. Goes on with the next batch while time is left
//
// Args:
//   - `deadline`: A README request starts only if it can end `readmeCommitReserve` before this
//   - `skippedIDs`: IDs of repositories that GitHub can't find now. Asking them would only make absent markers
//   - `now`: Time that picks today's group. Taken as an argument so tests can set the day
//
// Returns:
//   - The number of READMEs fetched and compared with the saved ones. Only the changes are committed
//   - An error from reading the archive or from a commit.
//     Running out of time or reaching the rate limit isn't an error: what was fetched is committed if time is left for the commit,
//     it is logged, and the next sync goes on
//
// NOTE: Snapshots of items no longer in the archive are deleted in every commit it makes.
// With nothing to check, it still makes one commit for them.
// So files that a deletion outside the server left behind (for example, a hand edit of `db/star-items.json`) go away.
func SyncReadmeSnapshotsWithGitHub(cfg *Config, deadline time.Time, skippedIDs []int64, now time.Time) (int, error) {
	commitSHA, err := readBranchCommit(cfg)
	if err != nil {
		return 0, err
	}
	starItems, err := readItems[StarItem](cfg, StarItemsDBPath)
	if err != nil {
		return 0, err
	}
	snapshotsByID, err := readReadmeSnapshotsInFolder(cfg, commitSHA, ReadmesDBDir)
	if err != nil {
		return 0, err
	}
	isSkipped := map[int64]bool{}
	for _, id := range skippedIDs {
		isSkipped[id] = true
	}
	todaysGroup := syncDayNumber(now) % readmeRefreshGroups
	uncheckedIDs := []int64{}
	todaysIDs := []int64{}
	for _, item := range starItems {
		switch {
		case isSkipped[item.ID]:
		case len(snapshotsByID[item.ID]) == 0:
			uncheckedIDs = append(uncheckedIDs, item.ID)
		case item.ID%readmeRefreshGroups == todaysGroup:
			todaysIDs = append(todaysIDs, item.ID)
		}
	}
	// Shuffled, so a run that can't check them all leaves different items for the next run
	rand.Shuffle(len(uncheckedIDs), func(i int, j int) { uncheckedIDs[i], uncheckedIDs[j] = uncheckedIDs[j], uncheckedIDs[i] })
	rand.Shuffle(len(todaysIDs), func(i int, j int) { todaysIDs[i], todaysIDs[j] = todaysIDs[j], todaysIDs[i] })
	queue := append(uncheckedIDs, todaysIDs...)

	fetchDeadline := deadline.Add(-readmeCommitReserve)
	checked := 0
	for {
		batch := queue[:min(maxReadmesPerCommit, len(queue))] // Can be empty: the commit still deletes the snapshots of removed items
		fetched, stopErr := fetchReadmesInParallel(cfg, batch, fetchDeadline)
		// A request can end right at `fetchDeadline`, so the time left can be a little under `readmeCommitReserve`.
		// Half of it is still enough for the commit, and the READMEs fetched aren't thrown away
		if time.Until(deadline) < readmeCommitReserve/2 {
			log.Printf("readmes: no time left to commit %d fetched READMEs", len(fetched))
			return checked, nil
		}
		if err := writeFetchedReadmesInSingleCommit(cfg, fetched); err != nil {
			return checked, err
		}
		checked += len(fetched)
		queue = queue[len(batch):]
		if stopErr != nil {
			log.Printf("readmes: stopped with %d READMEs left for the next sync: %v", len(queue)+len(batch)-len(fetched), stopErr)
			return checked, nil
		}
		if len(queue) == 0 {
			return checked, nil
		}
	}
}

// Error for README requests that weren't sent because the time ran out
var errReadmesOutOfTime = errors.New("out of time")

// Error for a batch whose README requests all failed.
// GitHub is likely failing for every repository, so asking more would only use up the rate limit
var errReadmesAllFailed = errors.New("every README request of a batch failed")

// Fetches the READMEs of the items `itemIDs`, `readmeFetchParallel` at a time.
// A request starts only if it can end by `fetchDeadline` even when it waits the longest (`githubTimeout`).
//
// Returns:
//   - The READMEs fetched: item ID -> README. An item whose request failed isn't in it, and is asked again later
//   - Why it stopped before asking every item: `errReadmesOutOfTime`, or an error that wraps `errGitHubRateLimited`.
//     `errReadmesAllFailed` if every request failed, so no more batches are asked.
//     `nil` if every item was asked and at least one answered
func fetchReadmesInParallel(cfg *Config, itemIDs []int64, fetchDeadline time.Time) (map[int64]FetchedReadme, error) {
	// Several goroutines (lightweight threads) take items one by one. Shared values are read and written while holding `mu`
	var mu sync.Mutex
	next := 0
	fetched := map[int64]FetchedReadme{}
	failed := 0
	var stopErr error
	var wg sync.WaitGroup // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < readmeFetchParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if stopErr == nil && next < len(itemIDs) && time.Now().Add(githubTimeout).After(fetchDeadline) {
					stopErr = errReadmesOutOfTime
				}
				if stopErr != nil || next >= len(itemIDs) {
					mu.Unlock()
					return
				}
				id := itemIDs[next]
				next++
				mu.Unlock()

				readme, err := FetchReadmeByRepoID(cfg, id, fetchDeadline)
				mu.Lock()
				switch {
				case errors.Is(err, errGitHubRateLimited):
					if stopErr == nil {
						stopErr = err
					}
				case err != nil:
					failed++
				default:
					fetched[id] = readme
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed > 0 {
		log.Printf("readmes: %d requests failed, asked again later", failed)
	}
	if stopErr == nil && failed > 0 && len(fetched) == 0 {
		stopErr = errReadmesAllFailed
	}
	return fetched, stopErr
}

// Writes the READMEs `fetched` in one commit, and deletes in it the snapshots of items no longer in the archive.
// It reads the archive and lists `ReadmesDBDir` at the commit it builds on, so items removed meanwhile are skipped.
// Nothing is committed if nothing changes.
func writeFetchedReadmesInSingleCommit(cfg *Config, fetched map[int64]FetchedReadme) error {
	return UpdateFiles(cfg, []string{StarItemsDBPath}, func(filesRead map[string][]byte, commitSHA string) (FileChangesCommit, error) {
		starItems, err := decodeItems[StarItem](filesRead[StarItemsDBPath])
		if err != nil {
			return FileChangesCommit{}, err
		}
		isArchived := make(map[int64]bool, len(starItems)) // Item ID -> `true`, for the items in the archive
		for _, item := range starItems {
			isArchived[item.ID] = true
		}
		snapshotsByID, err := readReadmeSnapshotsInFolder(cfg, commitSHA, ReadmesDBDir)
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
			if _, isWritten := changes.FilesToWrite[readme.buildPath(item.ID)]; isWritten {
				if readme.isAbsent {
					absentNames = append(absentNames, item.Name)
				} else {
					savedNames = append(savedNames, item.Name)
				}
			}
		}
		removedCount := 0 // Number of items no longer in the archive whose snapshots are deleted
		for id, snapshots := range snapshotsByID {
			if isArchived[id] {
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
