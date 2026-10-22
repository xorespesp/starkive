// README snapshots.
// The start of each archived repository's README is kept in its own file under `ReadmesDBDir`, so it stays after the repository is gone.
// This file covers where the files go (`FetchedReadme.buildPath`), finding them (`readReadmeSnapshotsInFolder`),
// what they hold (`buildReadmeSnapshotFile`, `parseReadmeSnapshotFile`), what saving one writes and deletes (`planReadmeSnapshotUpdate`),
// fetching READMEs from GitHub (`FetchReadmesByNodeIDs`),
// reading a saved one back (`ReadReadmeSnapshot`),
// and the sync step that keeps them up to date (`SyncReadmeSnapshotsWithGitHub`).
//
// NOTE: These files aren't JSON arrays like the other data files, on purpose.
// One file per repository keeps each change small.
// Like the metadata snapshots, they copy text that the repository owner can change, because keeping what it was is the point.
package core

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
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

// Makes the content of a README file from the README text that GitHub gives.
//   - A UTF-8 byte order mark at the start is removed
//   - Text over `maxReadmeBytes` is cut after its last line break in that range,
//     or at a character boundary if there is no line break or it would keep less than half of it.
//     Then `readmeCutLine` is added on a line of its own, ending with `\n`
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
	isCut := len(text) > maxReadmeBytes
	if isCut {
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
	if !isCut {
		return text, true
	}
	if !bytes.HasSuffix(text, []byte("\n")) {
		text = append(text, '\n')
	}
	return append(append(text, readmeCutLine...), '\n'), true
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

// Largest number of `node_id`s that one `FetchReadmesByNodeIDs` call takes.
// Its slowest query lists two folders of each repository.
// For 50 repositories that query took up to 7.3 seconds (measured),
// close to the 10 seconds after which GitHub ends a GraphQL query (docs).
// With 30, a whole call of up to three queries took 5.6 to 8.3 seconds (measured).
// A query that times out also costs extra rate limit points (measured).
const maxReadmeNodeIDs = 30

// One folder of a repository as GitHub GraphQL lists it (`Tree.entries`).
// A `*githubFolder` is `nil` if the folder doesn't exist
type githubFolder struct {
	Entries []githubFolderEntry `json:"entries"`
}

// One entry of a folder in `githubFolder`
type githubFolderEntry struct {
	Name string `json:"name"` // File or folder name
	Type string `json:"type"` // `blob` for a file, `tree` for a folder, `commit` for a submodule
	Mode int    `json:"mode"` // Git file mode in decimal (e.g. 33188 for `100644`, 40960 for a symbolic link `120000`)
}

// File name that counts as a README: `readme` alone or with one extension, in any case.
// GitHub doesn't document its rule (docs).
// On 300 repositories, this rule found the README GitHub shows in every one that has one (measured).
// A name with two dots (`README.ko.md`) is left out.
// GitHub never chose one over a one-dot name such as `README.md` (measured on the same 300).
// Whether GitHub shows one when it is the only README isn't measured
var readmeFileNamePattern = regexp.MustCompile(`(?i)^readme(\.[^.]+)?$`)

// Lists the READMEs in this folder, in the order GitHub picks among them.
// A Markdown file (`.md`, `.markdown`) comes first, then another extension, then no extension.
// A tie is broken by byte order, so `README.md` comes before `readme.md`.
// This matched GitHub's choice in all 9 folders with several READMEs (measured).
//
// NOTE: Only plain files count.
// A folder or a submodule with a README name isn't a README.
// A symbolic link is skipped too, because its content is the path it points to, not the README.
// It can be called on a `nil` folder (a folder that doesn't exist), and returns nothing.
// Go allows calling a method on a `nil` pointer, unlike C++, as long as the method doesn't read through it.
func (folder *githubFolder) listReadmeFileNames() []string {
	if folder == nil {
		return nil
	}
	rank := func(name string) int {
		extension := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
		switch {
		case extension == "md" || extension == "markdown":
			return 0
		case extension != "":
			return 1
		default:
			return 2
		}
	}
	names := []string{}
	for _, entry := range folder.Entries {
		if entry.Type == "blob" && entry.Mode != 0o120000 && readmeFileNamePattern.MatchString(entry.Name) {
			names = append(names, entry.Name)
		}
	}
	slices.SortFunc(names, func(a string, b string) int {
		if rank(a) != rank(b) {
			return rank(a) - rank(b)
		}
		return strings.Compare(a, b)
	})
	return names
}

// Path prefixes of the folders where GitHub looks for a README (docs).
// A file's path in the repository is the prefix followed by the file name
const (
	githubFolderPrefix = ".github/" // The `.github` folder
	rootFolderPrefix   = ""         // The root folder
	docsFolderPrefix   = "docs/"    // The `docs` folder
)

// README candidates of one repository while `FetchReadmesByNodeIDs` picks its README.
// They are the listings of the folders where GitHub looks, and the files ruled out as binary
type readmeCandidates struct {
	githubFolderListing *githubFolder   // Listing of the `.github` folder. `nil` if the folder doesn't exist
	rootFolderListing   *githubFolder   // Listing of the root folder. `nil` for an empty repository
	docsFolderListing   *githubFolder   // Listing of the `docs` folder. `nil` if the folder doesn't exist or isn't listed yet
	isDocsFolderListed  bool            // `true` once `docs` was listed. It is listed later, only when the first two folders have no README left
	binaryFilePaths     map[string]bool // Paths of README files that GitHub judged binary, which aren't READMEs
}

// Returns the path of the README that GitHub shows, from what is known so far.
// It looks in `.github`, then the root, then `docs`, the order GitHub looks in (docs).
//
// Returns:
//   - The path, or "" if no README is left in the three folders
//   - `true` if the `docs` folder must be listed before the README can be told
//
// NOTE: GitHub doesn't count a file it judges binary (`isBinary`) as a README.
// It shows the next README in the same folder, or one in the next folder (measured).
// So a binary file is passed over the same way, not taken as "no README".
func (candidates *readmeCandidates) pickReadmePath() (string, bool) {
	// Returns the path of the first README in folder `listing` that isn't ruled out as binary, or "" if there is none.
	// `folderPrefix` is the path prefix of the folder (`githubFolderPrefix` and so on)
	pickFirstReadmePath := func(folderPrefix string, listing *githubFolder) string {
		for _, name := range listing.listReadmeFileNames() {
			if !candidates.binaryFilePaths[folderPrefix+name] {
				return folderPrefix + name
			}
		}
		return ""
	}
	if readmePath := pickFirstReadmePath(githubFolderPrefix, candidates.githubFolderListing); readmePath != "" {
		return readmePath, false
	}
	if readmePath := pickFirstReadmePath(rootFolderPrefix, candidates.rootFolderListing); readmePath != "" {
		return readmePath, false
	}
	if !candidates.isDocsFolderListed {
		return "", true
	}
	return pickFirstReadmePath(docsFolderPrefix, candidates.docsFolderListing), false
}

// Most content queries one `FetchReadmesByNodeIDs` call sends.
// A query after the first is sent only for repositories whose picked file was binary, which is rare (none in 1,350 READMEs, measured).
// This keeps a call short even if a repository has many binary READMEs
const maxReadmeContentQueries = 3

// Fetches the READMEs of the repositories `nodeIDs` with GitHub GraphQL, ending by `deadline`.
// It picks each README the way GitHub does (`readmeCandidates.pickReadmePath`), and reads the picked files.
// That is usually up to three queries, sent one after another (the listing, the `docs` listing if needed, the contents).
// A binary pick is passed over, which takes more queries (`maxReadmeContentQueries`).
//
// Args:
//   - `nodeIDs`: `node_id`s of the repositories. At most `maxReadmeNodeIDs`
//
// Returns:
//   - READMEs in the same order and length as `nodeIDs`.
//     A README is absent (`isAbsent`) when GitHub says the repository isn't found,
//     when none of the three folders has a README that isn't binary, or when the README is blank (`buildReadmeSnapshotFile`).
//     `nil` where it can't tell now: an ID that isn't a repository, a repository or folder that GraphQL can't give for a reason other than `NOT_FOUND`,
//     a file gone between the queries, or a repository still left after `maxReadmeContentQueries` or after a later query failed.
//     Ask those again later
//   - An error that wraps `errGitHubRateLimited` if GitHub says the rate limit was reached (`callGraphQL`).
//     Any other error means a query failed or wasn't sent (`errDeadlinePassed`), and says nothing about the READMEs
//
// NOTE: It asks by `node_id`, so a renamed repository needs no lookup (measured).
// GraphQL gives `text` in UTF-8.
// GitHub turns older encodings (EUC-KR, Shift_JIS) and UTF-16 with a byte order mark into UTF-8 (measured).
// A file GitHub judges binary by its content (`isBinary`) comes with no `text` (measured).
// UTF-16 without a byte order mark is binary or not by its content, and when it isn't, its `text` keeps the NUL bytes (measured).
// Text over 512,000 bytes comes cut (measured), which still holds more than `maxReadmeBytes`.
func FetchReadmesByNodeIDs(cfg *Config, nodeIDs []string, deadline time.Time) ([]*FetchedReadme, error) {
	// Lists the `.github` folder and the root folder.
	// GitHub looks for a README in `.github`, then the root, then `docs` (docs).
	// Most READMEs are in the first two (measured on 300 repositories), so `docs` is listed later, only when needed
	const listingQuery = `query($ids: [ID!]!) {
		nodes(ids: $ids) {
			... on Repository {
				id
				github: object(expression: "HEAD:.github") { ...folderEntries }
				root: object(expression: "HEAD:") { ...folderEntries }
			}
		}
	}
	fragment folderEntries on Tree { entries { name type mode } }`
	var listed struct {
		Nodes []*struct { // `nil` for a repository GraphQL can't give. The reason is in the errors
			ID     string        `json:"id"`     // Empty for an ID that isn't a repository (GraphQL gives `{}`)
			GitHub *githubFolder `json:"github"` // The `.github` folder
			Root   *githubFolder `json:"root"`   // The root folder. `nil` for an empty repository
		} `json:"nodes"`
	}
	timeLeft, err := timeLeftUntil(deadline)
	if err != nil {
		return nil, err
	}
	err = callGraphQL(cfg, listingQuery, map[string]any{"ids": nodeIDs}, timeLeft, &listed)
	var nodeErrs graphQLErrors // Errors about some repositories, when the rest of the answer can be used
	if err != nil && !errors.As(err, &nodeErrs) {
		return nil, err
	}
	if len(listed.Nodes) != len(nodeIDs) { // If positions don't line up, we can't tell which answer belongs to which repository
		return nil, Errorf(http.StatusBadGateway, "GitHub GraphQL returned %d nodes for %d ids", len(listed.Nodes), len(nodeIDs))
	}
	readmes := make([]*FetchedReadme, len(nodeIDs))
	candidatesByRepo := make([]*readmeCandidates, len(nodeIDs)) // README candidates of each repository still being picked, or `nil` once its result is settled
	for i, node := range listed.Nodes {
		switch {
		case node == nil:
			if nodeErrs.isNodeNotFound(i) {
				readmes[i] = &FetchedReadme{isAbsent: true}
			}
		case node.ID == "": // Not a repository (GraphQL gives `{}`). It stays `nil`, and is asked again later
		case nodeErrs.hasErrorInNode(i): // A folder that failed looks like a missing one, so the README can't be told now. It stays `nil`
		default:
			candidatesByRepo[i] = &readmeCandidates{githubFolderListing: node.GitHub, rootFolderListing: node.Root, binaryFilePaths: map[string]bool{}}
		}
	}

	for contentQueries := 0; contentQueries < maxReadmeContentQueries; contentQueries++ {
		// Lists `docs` for the repositories whose first two folders have no README left
		// A query after the first content query is only for repositories whose pick was binary.
		// If it fails, those stay `nil` and are asked again later, and the READMEs already settled are kept
		isFirstRound := contentQueries == 0
		docsIndexes := []int{} // Positions in `nodeIDs` of the repositories whose `docs` folder is listed now
		for i, candidates := range candidatesByRepo {
			if candidates == nil {
				continue
			}
			if _, needsDocs := candidates.pickReadmePath(); needsDocs {
				docsIndexes = append(docsIndexes, i)
			}
		}
		if len(docsIndexes) > 0 {
			docsFolders, isListed, err := listReadmeDocsFolders(cfg, nodeIDs, docsIndexes, deadline)
			if err != nil && isFirstRound {
				return nil, err
			}
			if err != nil {
				break
			}
			for k, i := range docsIndexes {
				if !isListed[k] { // The repository or its `docs` folder failed this time, so it stays `nil` and is asked again later
					candidatesByRepo[i] = nil
					continue
				}
				candidatesByRepo[i].docsFolderListing, candidatesByRepo[i].isDocsFolderListed = docsFolders[k], true
			}
		}

		pickedPaths := map[int]string{} // Position in `nodeIDs` -> path of the README picked to read now
		for i, candidates := range candidatesByRepo {
			if candidates == nil {
				continue
			}
			pickedPath, _ := candidates.pickReadmePath() // Every folder is listed by now
			if pickedPath == "" {
				readmes[i], candidatesByRepo[i] = &FetchedReadme{isAbsent: true}, nil
				continue
			}
			pickedPaths[i] = pickedPath
		}
		if len(pickedPaths) == 0 {
			break
		}
		files, err := readReadmeFiles(cfg, nodeIDs, pickedPaths, deadline)
		if err != nil && isFirstRound {
			return nil, err
		}
		if err != nil {
			break
		}
		for i, pickedPath := range pickedPaths {
			file := files[i]
			switch {
			case file == nil: // The repository or the file is gone, or GraphQL couldn't give it, so it stays `nil` and is asked again later
				candidatesByRepo[i] = nil
			case file.IsBinary: // Not a README to GitHub, so the next pick is read in the next query
				candidatesByRepo[i].binaryFilePaths[pickedPath] = true
			case file.Text == nil: // Every file that wasn't binary came with text (measured), so this isn't expected and stays `nil`
				candidatesByRepo[i] = nil
			default:
				readmes[i], candidatesByRepo[i] = buildFetchedReadme(pickedPath, *file.Text), nil
			}
		}
	}
	return readmes, nil
}

// Lists the `docs` folder of the repositories at positions `indexes` of `nodeIDs`, with one query that ends by `deadline`.
//
// Returns:
//   - The `docs` folder for each position, in the same order. `nil` if the folder doesn't exist
//   - `true` for each position that was listed.
//     `false` where GraphQL couldn't give the repository or the folder this time (for example, it went away after the first query)
//   - An error if the query failed, as for `FetchReadmesByNodeIDs`
func listReadmeDocsFolders(cfg *Config, nodeIDs []string, indexes []int, deadline time.Time) ([]*githubFolder, []bool, error) {
	ids := []string{}
	for _, i := range indexes {
		ids = append(ids, nodeIDs[i])
	}
	const docsListingQuery = `query($ids: [ID!]!) {
		nodes(ids: $ids) {
			... on Repository {
				docs: object(expression: "HEAD:docs") { ...folderEntries }
			}
		}
	}
	fragment folderEntries on Tree { entries { name type mode } }`
	var docsListed struct {
		Nodes []*struct {
			Docs *githubFolder `json:"docs"` // The `docs` folder
		} `json:"nodes"`
	}
	timeLeft, err := timeLeftUntil(deadline)
	if err != nil {
		return nil, nil, err
	}
	err = callGraphQL(cfg, docsListingQuery, map[string]any{"ids": ids}, timeLeft, &docsListed)
	var nodeErrs graphQLErrors // Errors about some repositories, when the rest of the answer can be used
	if err != nil && !errors.As(err, &nodeErrs) {
		return nil, nil, err
	}
	if len(docsListed.Nodes) != len(ids) {
		return nil, nil, Errorf(http.StatusBadGateway, "GitHub GraphQL returned %d nodes for %d ids", len(docsListed.Nodes), len(ids))
	}
	folders := make([]*githubFolder, len(ids))
	isListed := make([]bool, len(ids))
	for k, node := range docsListed.Nodes {
		if node != nil && !nodeErrs.hasErrorInNode(k) { // A `docs` folder that failed can't be told from a missing one, so it isn't counted as listed
			folders[k], isListed[k] = node.Docs, true
		}
	}
	return folders, isListed, nil
}

// One file as GitHub GraphQL gives it (`Blob`, the fields in use)
type githubFile struct {
	IsBinary bool    `json:"isBinary"` // `true` if GitHub judged the file binary by its content. Then `Text` is `nil`
	Text     *string `json:"text"`     // The text in UTF-8. `nil` for a binary file
}

// Reads the files at `pickedPaths` (position in `nodeIDs` -> path) with one query that ends by `deadline`.
//
// Returns:
//   - Position -> the file. `nil` where GraphQL couldn't give the repository or the file is gone
//   - An error if the query failed, as for `FetchReadmesByNodeIDs`
func readReadmeFiles(cfg *Config, nodeIDs []string, pickedPaths map[int]string, deadline time.Time) (map[int]*githubFile, error) {
	// Each repository needs its own file path, so each gets its own aliased field (`r<position>`) with its own variables
	fields := []string{}
	params := []string{}
	variables := map[string]any{}
	for i, pickedPath := range pickedPaths {
		fields = append(fields, fmt.Sprintf("r%d: node(id: $id%d) { ... on Repository { object(expression: $path%d) { ... on Blob { isBinary text } } } }", i, i, i))
		params = append(params, fmt.Sprintf("$id%d: ID!, $path%d: String!", i, i))
		variables[fmt.Sprintf("id%d", i)] = nodeIDs[i]
		variables[fmt.Sprintf("path%d", i)] = "HEAD:" + pickedPath
	}
	contentQuery := fmt.Sprintf("query(%s) { %s }", strings.Join(params, ", "), strings.Join(fields, " "))
	var contents map[string]*struct { // Alias (`r<position>`) -> repository. `nil` if GraphQL can't give it
		Object *githubFile `json:"object"` // `nil` if the file is gone
	}
	timeLeft, err := timeLeftUntil(deadline)
	if err != nil {
		return nil, err
	}
	err = callGraphQL(cfg, contentQuery, variables, timeLeft, &contents)
	var contentErrs graphQLErrors // A repository that failed is `nil`, and is asked again later
	if err != nil && !errors.As(err, &contentErrs) {
		return nil, err
	}
	files := map[int]*githubFile{}
	for i := range pickedPaths {
		if repo := contents[fmt.Sprintf("r%d", i)]; repo != nil {
			files[i] = repo.Object
		}
	}
	return files, nil
}

// Makes the README to save from the text of the file at `readmePath`: absent if the text is blank, otherwise the file content with its extension
func buildFetchedReadme(readmePath string, text string) *FetchedReadme {
	content, isUsable := buildReadmeSnapshotFile(text)
	if !isUsable {
		return &FetchedReadme{isAbsent: true}
	}
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(readmePath), "."))
	if !readmeExtensionPattern.MatchString(extension) {
		extension = "" // An extension that isn't allowed is dropped, and the file is saved as `<ID>`
	}
	return &FetchedReadme{extension: extension, content: content}
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
	Text      string `json:"text,omitempty"`      // The saved README text, without `readmeCutLine`. Bytes that aren't UTF-8 (only in a file the server didn't write, such as a hand edit) become U+FFFD in JSON
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
		return ReadmeSnapshotContent{State: readmeStateSaved, Format: extension, Text: text, Truncated: isTruncated}, nil
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

// Number of `FetchReadmesByNodeIDs` calls sent at the same time during sync.
// One call for `maxReadmeNodeIDs` repositories took 5.6 to 8.3 seconds (measured).
// 3 at once checked 300 repositories in 29.6 seconds, with no secondary rate limit (measured).
// GitHub allows 60 seconds of GraphQL CPU time per minute (docs), so more at once could reach it
const readmeFetchParallel = 3

// Time kept before the deadline for the commit after the last README call of a batch.
// A commit of `maxReadmesPerCommit` READMEs makes a tree that took about 4 seconds (measured),
// so this leaves room for a slow commit
const readmeCommitReserve = 2 * githubTimeout

// Brings the README snapshots of the archive in line with the READMEs on GitHub, until `deadline`.
//  1. Picks the items to check: items with no snapshot first, then today's group (`readmeRefreshGroups`), each in random order.
//     Items in `skippedIDs` aren't asked
//  2. Fetches up to `maxReadmesPerCommit` READMEs (`fetchReadmesInParallel`), and writes them in one commit (`writeFetchedReadmesInSingleCommit`)
//  3. Goes on with the next batch while time is left
//
// Args:
//   - `deadline`: A README call starts only if its first query can end `readmeCommitReserve` before this
//   - `skippedIDs`: IDs of repositories that GitHub can't find now. Asking them would only make absent markers
//   - `now`: Time that picks today's group. Taken as an argument so tests can set the day
//
// Returns:
//   - The number of READMEs fetched and compared with the saved ones. Only the changes are committed
//   - An error from reading the archive or from a commit.
//     Running out of time, reaching the rate limit or every call of a batch failing isn't an error: what was fetched is committed if time is left for the commit,
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
	uncheckedItems := []StarItem{}
	todaysItems := []StarItem{}
	for _, item := range starItems {
		switch {
		case isSkipped[item.ID]:
		case item.NodeID == "": // An item without `node_id` can't be asked about with GraphQL
		case len(snapshotsByID[item.ID]) == 0:
			uncheckedItems = append(uncheckedItems, item)
		case item.ID%readmeRefreshGroups == todaysGroup:
			todaysItems = append(todaysItems, item)
		}
	}
	// Shuffled, so a run that can't check them all leaves different items for the next run
	rand.Shuffle(len(uncheckedItems), func(i int, j int) { uncheckedItems[i], uncheckedItems[j] = uncheckedItems[j], uncheckedItems[i] })
	rand.Shuffle(len(todaysItems), func(i int, j int) { todaysItems[i], todaysItems[j] = todaysItems[j], todaysItems[i] })
	queue := append(uncheckedItems, todaysItems...)

	fetchDeadline := deadline.Add(-readmeCommitReserve)
	checked := 0
	for {
		batch := queue[:min(maxReadmesPerCommit, len(queue))] // Can be empty: the commit still deletes the snapshots of removed items
		fetched, stopErr := fetchReadmesInParallel(cfg, batch, fetchDeadline)
		// A call can end right at `fetchDeadline`, so the time left can be a little under `readmeCommitReserve`.
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

// Error for README calls that weren't sent because the time ran out
var errReadmesOutOfTime = errors.New("out of time")

// Error for a batch whose README calls all failed.
// GitHub is likely failing as a whole, so asking more would only use up the rate limit
var errReadmesAllFailed = errors.New("every README call of a batch failed")

// Fetches the READMEs of the items `items`, `maxReadmeNodeIDs` in each `FetchReadmesByNodeIDs` call and `readmeFetchParallel` calls at a time.
// A call starts only if its first query can end by `fetchDeadline` even when it waits the longest (`githubTimeout`).
//
// Returns:
//   - The READMEs fetched: item ID -> README.
//     An item that GitHub can't tell about now, or whose call failed, isn't in it, and is asked again later
//   - Why it stopped before asking every item: `errReadmesOutOfTime`, or an error that wraps `errGitHubRateLimited`.
//     `errReadmesAllFailed` if every call failed, so no more batches are asked.
//     `nil` if every item was asked and at least one call succeeded
//
// NOTE: A call usually has up to three queries in a row, each ending in a few seconds as a rule (measured).
// One that runs past `fetchDeadline` fails, and its items are asked again later.
// A failure about one repository isn't a failed call: GraphQL answers the others, and that one isn't fetched.
func fetchReadmesInParallel(cfg *Config, items []StarItem, fetchDeadline time.Time) (map[int64]FetchedReadme, error) {
	groups := [][]StarItem{} // `items` split into the items of each call
	for start := 0; start < len(items); start += maxReadmeNodeIDs {
		groups = append(groups, items[start:min(start+maxReadmeNodeIDs, len(items))])
	}
	// Several goroutines (lightweight threads) take groups one by one. Shared values are read and written while holding `mu`
	var mu sync.Mutex
	next := 0
	fetched := map[int64]FetchedReadme{}
	failed := 0
	succeeded := 0
	var stopErr error
	var wg sync.WaitGroup // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < readmeFetchParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if stopErr == nil && next < len(groups) && time.Now().Add(githubTimeout).After(fetchDeadline) {
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
				readmes, err := FetchReadmesByNodeIDs(cfg, nodeIDs, fetchDeadline)
				mu.Lock()
				switch {
				case errors.Is(err, errGitHubRateLimited):
					if stopErr == nil {
						stopErr = err
					}
				case err != nil:
					failed++
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
