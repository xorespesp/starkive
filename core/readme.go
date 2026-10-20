// README snapshots.
// The start of each archived repository's README is kept in its own file under `ReadmesDBDir`, so it stays after the repository is gone.
// This file covers where the files go (`FetchedReadme.buildPath`), finding them (`readReadmeSnapshotsInFolder`),
// and what they hold (`buildReadmeFile`, `parseReadmeFile`).
//
// NOTE: These files aren't JSON arrays like the other data files, on purpose.
// One file per repository keeps each change small.
// Like the metadata snapshots, they copy text that the repository owner can change, because keeping what it was is the point.
package core

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	isAbsent  bool   // `true` if the repository has no README, or one that isn't usable text (`buildReadmeFile`). It is saved as the marker (`readmeAbsentMarkerSuffix`)
	extension string // Extension of the README file name (`readmeExtensionPattern`). Empty if it has none or it isn't allowed
	content   []byte // File content made by `buildReadmeFile`. Empty if `isAbsent`
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
func buildReadmeFile(sourceBytes []byte) ([]byte, bool) {
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

// Splits the content of a README file into the README text and whether it was cut (`buildReadmeFile` added `readmeCutLine`).
//
// NOTE: A README that isn't cut but itself ends with `readmeCutLine` reads as cut.
// The line names starkive, so no real README is expected to end with it.
func parseReadmeFile(content []byte) (string, bool) {
	text, endsWithCutLine := bytes.CutSuffix(content, []byte(readmeCutLine+"\n"))
	if !endsWithCutLine || (len(text) > 0 && !bytes.HasSuffix(text, []byte("\n"))) {
		return string(content), false
	}
	return string(text), true
}
