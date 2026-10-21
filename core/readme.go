// README snapshots.
// The start of each archived repository's README is kept in its own file under `ReadmesDBDir`, so it stays after the repository is gone.
// This file covers where the files go (`FetchedReadme.buildPath`), finding them (`readReadmeSnapshotsInFolder`),
// what they hold (`buildReadmeSnapshotFile`, `parseReadmeSnapshotFile`), what saving one writes and deletes (`planReadmeSnapshotUpdate`),
// fetching a README from GitHub (`FetchReadmeByRepoID`), and reading a saved one back (`ReadReadmeSnapshot`).
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
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// Line added at the end of a README that was cut at `maxReadmeBytes`. It isn't counted in `maxReadmeBytes`
const readmeCutLine = "[starkive: the rest of this README was not saved]"

// End of the file name of the marker that says a repository was checked and its README is absent (`<ID>.no-readme`, empty content).
// It contains `-`, so it never matches a README extension (`readmeExtensionPattern`)
const readmeAbsentMarkerSuffix = ".no-readme"

// Allowed extension of a README snapshot file name.
// A file with any other extension isn't a README snapshot
var readmeExtensionPattern = regexp.MustCompile(`^[a-z0-9]{1,10}$`)

// README of one repository as fetched from GitHub: the content to save, or the fact that the README is absent
type FetchedReadme struct {
	isAbsent  bool   // `true` if the repository has no README, or one that isn't usable text (`buildReadmeSnapshotFile`). It is saved as the marker (`readmeAbsentMarkerSuffix`)
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
	isAbsent bool   // `true` if the file is the marker that says the README is absent (`readmeAbsentMarkerSuffix`)
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
//     Every other snapshot of the item (an old extension, the marker) is deleted, so the item keeps one file
//   - An absent README keeps a saved README, because keeping what it was is the point.
//     Only a marker next to it is deleted
//   - An absent README of an item with no saved README is written as the marker
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
//   - Text with a UTF-16 byte order mark is turned into UTF-8. A UTF-8 byte order mark is removed
//   - Text over `maxReadmeBytes` is cut after its last line break in that range, or at a character boundary if there is none.
//     Then `readmeCutLine` is added on a line of its own, ending with `\n`
//
// Args:
//   - `sourceBytes`: The README as GitHub stores it.
//     It can be only the start of the file, if that start is longer than `maxReadmeBytes` after it is turned into UTF-8
//
// Returns:
//   - The file content, and `false` if the README isn't usable text (blank, has a NUL byte, or isn't valid UTF-8)
//
// NOTE: Only the kept part is checked for UTF-8, so a character cut off at the end of `sourceBytes` doesn't matter.
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
	isCut := len(text) > maxReadmeBytes
	if isCut {
		cutAt := maxReadmeBytes
		for i := 1; i < utf8.UTFMax && cutAt > 0 && !utf8.RuneStart(text[cutAt]); i++ { // Moves back to the first byte of the character at `cutAt`, so no character is split
			cutAt--
		}
		text = text[:cutAt]
		if lastLineBreak := bytes.LastIndexByte(text, '\n'); lastLineBreak >= 0 {
			text = text[:lastLineBreak+1]
		}
	}
	if len(bytes.TrimSpace(text)) == 0 || bytes.IndexByte(text, 0) >= 0 || !utf8.Valid(text) {
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
//     or when the README isn't usable text (`buildReadmeSnapshotFile`)
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
	readmeStateAbsent    = "absent"    // Checked, with no usable README (the marker)
	readmeStateUnchecked = "unchecked" // Not checked yet (no file)
)

// README snapshot of one item as read back: its state, and the README if one is saved
type ReadmeSnapshotContent struct {
	State     string `json:"state"`               // `saved`, `absent` or `unchecked` (`readmeStateSaved` and so on)
	Format    string `json:"format,omitempty"`    // Extension of the saved file (e.g. `md`). Empty if the file has none
	Text      string `json:"text,omitempty"`      // The saved README text, without `readmeCutLine`
	Truncated bool   `json:"truncated,omitempty"` // `true` if the README was cut at `maxReadmeBytes`
}

// Reads the README snapshot of item `itemID` at the branch tip.
// It reads the commit of the branch tip, lists the item's folder, and reads the file only when a README is saved.
// That is three requests at most.
//
// Returns:
//   - The snapshot. `unchecked` for an item with no file, which includes an ID that isn't in the archive.
//     A GitHub error, or a 502 `HTTPError` if a file in the listing can't be read at the same commit
//
// NOTE: The code always leaves one file per item, but a hand edit of the DB repository can leave more.
// A saved README wins over the marker, because a saved README is what this feature keeps.
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
		return ReadmeSnapshotContent{State: readmeStateSaved, Format: extension, Text: text, Truncated: isTruncated}, nil
	}
	return ReadmeSnapshotContent{State: readmeStateAbsent}, nil
}
