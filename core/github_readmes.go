// Fetching READMEs from GitHub with GitHub GraphQL (`FetchReadmesByNodeIDs`).
// It picks each repository's README the way GitHub does (`readmeCandidates.pickReadmePath`) and reads its text.
// Saving them as snapshots in the archive is in `archive_readmes.go`.
package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
)

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

// Names of the folders other than the root where GitHub looks for a README, in lower case (docs).
// GitHub finds them in any case, such as `.GitHub`, `Docs` or `DOCS` (measured)
const (
	githubFolderName = ".github"
	docsFolderName   = "docs"
)

// Returns the name of the subfolder of this folder whose name is `lowerName` in any case, or "" if there is none.
// Among several (`Docs` and `docs`), it returns the first in byte order, which is the one GitHub uses (measured).
// It can be called on a `nil` folder, like `listReadmeFileNames`.
func (folder *githubFolder) findSubfolderName(lowerName string) string {
	if folder == nil {
		return ""
	}
	found := ""
	for _, entry := range folder.Entries {
		if entry.Type == "tree" && strings.EqualFold(entry.Name, lowerName) && (found == "" || entry.Name < found) {
			found = entry.Name
		}
	}
	return found
}

// One folder where GitHub looks for a README, as known while `FetchReadmesByNodeIDs` picks the README
type readmeFolder struct {
	pathPrefix string        // Path of the folder with its own case and a trailing `/` (e.g. `Docs/`), or "" for the root
	listing    *githubFolder // Listing of the folder. `nil` if the folder doesn't exist
	isListed   bool          // `false` while the folder exists but isn't listed yet
}

// Returns the folder named `lowerName` in any case in the root folder `root`, the way GitHub finds it.
// A folder that doesn't exist counts as listed, with no listing
func newReadmeFolder(root *githubFolder, lowerName string) readmeFolder {
	name := root.findSubfolderName(lowerName)
	if name == "" {
		return readmeFolder{isListed: true}
	}
	return readmeFolder{pathPrefix: name + "/"}
}

// README candidates of one repository while `FetchReadmesByNodeIDs` picks its README.
// They are the folders where GitHub looks, and the files ruled out as binary
type readmeCandidates struct {
	dotGitHubFolder readmeFolder    // The `.github` folder in any case. The first query lists it when it has exactly this name
	rootFolder      readmeFolder    // The root folder, always listed. Its listing is `nil` for an empty repository
	docsFolder      readmeFolder    // The `docs` folder in any case, listed only when the first two folders have no README left
	binaryFilePaths map[string]bool // Paths of README files that GitHub judged binary, which aren't READMEs
}

// Returns the path of the README that GitHub shows, from what is known so far.
// It looks in `.github`, then the root, then `docs`, the order GitHub looks in (docs).
//
// Returns:
//   - The path, or "" if no README is left in the three folders or a folder must be listed first
//   - The folders that must be listed before the README can be told.
//     They are the folders not listed yet that come before the first README known to be left.
//     Pointers into `candidates`, so the listings can be filled in
//
// NOTE: GitHub doesn't count a file it judges binary (`isBinary`) as a README.
// It shows the next README in the same folder, or one in the next folder (measured, except from the root to `docs`).
// So a binary file is passed over the same way, not taken as "no README".
func (candidates *readmeCandidates) pickReadmePath() (string, []*readmeFolder) {
	unlisted := []*readmeFolder{}
	for _, folder := range []*readmeFolder{&candidates.dotGitHubFolder, &candidates.rootFolder, &candidates.docsFolder} {
		if !folder.isListed {
			unlisted = append(unlisted, folder)
			continue
		}
		for _, name := range folder.listing.listReadmeFileNames() {
			if candidates.binaryFilePaths[folder.pathPrefix+name] {
				continue
			}
			if len(unlisted) > 0 { // A folder before this one may hold the README
				return "", unlisted
			}
			return folder.pathPrefix + name, nil
		}
	}
	return "", unlisted
}

// Most content queries one `FetchReadmesByNodeIDs` call sends.
// A query after the first is sent only for repositories whose picked file was binary, which is rare (none in 1,350 READMEs, measured).
// This keeps a call short even if a repository has many binary READMEs
const maxReadmeContentQueries = 3

// Fetches the READMEs of the repositories `nodeIDs` with GitHub GraphQL, ending by the deadline of `ctx`.
// It picks each README the way GitHub does (`readmeCandidates.pickReadmePath`), and reads the picked files.
// That is usually up to three queries, sent one after another (the listing, the listing of another folder if needed, the contents).
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
//     a file gone between the queries, or a repository whose next pick is still unread after `maxReadmeContentQueries` content queries or after a later query failed.
//     Ask those again later
//   - An error that wraps `errGitHubRateLimited` if GitHub says the rate limit was reached before the first content query is answered (`callGraphQL`).
//     Any other error means a query failed, including one cut at the deadline (`context.DeadlineExceeded`), and says nothing about the READMEs.
//     If a later query fails, even for the rate limit, the READMEs settled so far are returned without an error
//
// NOTE: It asks by `node_id`, so a renamed repository needs no lookup (measured).
// GraphQL gives `text` in UTF-8.
// GitHub turns older encodings (EUC-KR, Shift_JIS) and UTF-16 with a byte order mark into UTF-8 (measured).
// A file GitHub judges binary by its content (`isBinary`) comes with no `text` (measured).
// UTF-16 without a byte order mark is binary or not by its content, and when it isn't, its `text` keeps the NUL bytes (measured).
// Text over 512,000 bytes comes cut (measured), which still holds more than `maxReadmeBytes`.
func FetchReadmesByNodeIDs(ctx context.Context, cfg *EnvConfig, nodeIDs []string) ([]*FetchedReadme, error) {
	// Lists the `.github` folder and the root folder.
	// GitHub looks for a README in `.github`, then the root, then `docs` (docs).
	// Most READMEs are in the first two (measured on 300 repositories), so `docs` is listed later, only when needed.
	// The root listing also tells whether `docs` exists and how its name is cased.
	// A `.github` folder in another case (`.GitHub`) is listed later too
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
			GitHub *githubFolder `json:"github"` // The folder named exactly `.github`
			Root   *githubFolder `json:"root"`   // The root folder. `nil` for an empty repository
		} `json:"nodes"`
	}
	err := callGraphQL(ctx, cfg, listingQuery, map[string]any{"ids": nodeIDs}, &listed)
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
			candidates := &readmeCandidates{
				dotGitHubFolder: newReadmeFolder(node.Root, githubFolderName),
				rootFolder:      readmeFolder{listing: node.Root, isListed: true},
				docsFolder:      newReadmeFolder(node.Root, docsFolderName),
				binaryFilePaths: map[string]bool{},
			}
			if candidates.dotGitHubFolder.pathPrefix == githubFolderName+"/" { // The first query listed it by this name
				candidates.dotGitHubFolder.listing, candidates.dotGitHubFolder.isListed = node.GitHub, true
			}
			candidatesByRepo[i] = candidates
		}
	}

	for contentQueries := 0; contentQueries < maxReadmeContentQueries; contentQueries++ {
		// A query after the first content query is only for repositories whose pick was binary.
		// If it fails, those stay `nil` and are asked again later, and the READMEs already settled are kept
		isFirstRound := contentQueries == 0
		foldersToList := map[int][]*readmeFolder{} // Position in `nodeIDs` -> folders of that repository listed now
		for i, candidates := range candidatesByRepo {
			if candidates == nil {
				continue
			}
			if _, unlisted := candidates.pickReadmePath(); len(unlisted) > 0 {
				foldersToList[i] = unlisted
			}
		}
		if len(foldersToList) > 0 {
			listings, err := listReadmeFolders(ctx, cfg, nodeIDs, foldersToList)
			if err != nil && isFirstRound {
				return nil, err
			}
			if err != nil {
				break
			}
			for i, folders := range foldersToList {
				for k, folder := range folders {
					// The root listed the folder, so a missing listing means it failed or went away this time.
					// The repository stays `nil` and is asked again later
					if listings[i][k] == nil {
						candidatesByRepo[i] = nil
						break
					}
					folder.listing, folder.isListed = listings[i][k], true
				}
			}
		}

		pickedPaths := map[int]string{} // Position in `nodeIDs` -> path of the README picked to read now
		for i, candidates := range candidatesByRepo {
			if candidates == nil {
				continue
			}
			pickedPath, _ := candidates.pickReadmePath() // Every folder it needs is listed by now
			if pickedPath == "" {
				readmes[i], candidatesByRepo[i] = &FetchedReadme{isAbsent: true}, nil
				continue
			}
			pickedPaths[i] = pickedPath
		}
		if len(pickedPaths) == 0 {
			break
		}
		files, err := readReadmeFiles(ctx, cfg, nodeIDs, pickedPaths)
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
	// A repository whose last pick was binary may have no README left in the three folders.
	// That is known without another query, so it is absent rather than asked again later
	for i, candidates := range candidatesByRepo {
		if candidates == nil {
			continue
		}
		if pickedPath, unlisted := candidates.pickReadmePath(); pickedPath == "" && len(unlisted) == 0 {
			readmes[i] = &FetchedReadme{isAbsent: true}
		}
	}
	return readmes, nil
}

// Lists the folders `foldersToList` (position in `nodeIDs` -> folders of that repository) with one query.
//
// Returns:
//   - Position -> the listing of each folder, in the same order.
//     `nil` where GraphQL couldn't give the repository or the folder this time (for example, it went away after the first query)
//   - An error if the query failed, as for `FetchReadmesByNodeIDs`
func listReadmeFolders(ctx context.Context, cfg *EnvConfig, nodeIDs []string, foldersToList map[int][]*readmeFolder) (map[int][]*githubFolder, error) {
	// Each folder has its own path, so each gets its own aliased field (`r<position>f<k>`) with its own path variable
	fields := []string{}
	params := []string{}
	variables := map[string]any{}
	for i, folders := range foldersToList {
		params = append(params, fmt.Sprintf("$id%d: ID!", i))
		variables[fmt.Sprintf("id%d", i)] = nodeIDs[i]
		for k, folder := range folders {
			fields = append(fields, fmt.Sprintf("r%df%d: node(id: $id%d) { ... on Repository { object(expression: $path%df%d) { ... on Tree { entries { name type mode } } } } }", i, k, i, i, k))
			params = append(params, fmt.Sprintf("$path%df%d: String!", i, k))
			variables[fmt.Sprintf("path%df%d", i, k)] = "HEAD:" + strings.TrimSuffix(folder.pathPrefix, "/")
		}
	}
	folderQuery := fmt.Sprintf("query(%s) { %s }", strings.Join(params, ", "), strings.Join(fields, " "))
	var answers map[string]*struct { // Alias (`r<position>f<k>`) -> repository. `nil` if GraphQL can't give it
		Object *githubFolder `json:"object"` // `nil` if the folder failed or is gone
	}
	err := callGraphQL(ctx, cfg, folderQuery, variables, &answers)
	var folderErrs graphQLErrors // A repository or folder that failed is `nil`, and is asked again later
	if err != nil && !errors.As(err, &folderErrs) {
		return nil, err
	}
	listings := map[int][]*githubFolder{}
	for i, folders := range foldersToList {
		listings[i] = make([]*githubFolder, len(folders))
		for k := range folders {
			if repo := answers[fmt.Sprintf("r%df%d", i, k)]; repo != nil {
				listings[i][k] = repo.Object
			}
		}
	}
	return listings, nil
}

// One file as GitHub GraphQL gives it (`Blob`, the fields in use)
type githubFile struct {
	IsBinary bool    `json:"isBinary"` // `true` if GitHub judged the file binary by its content. Then `Text` is `nil`
	Text     *string `json:"text"`     // The text in UTF-8. `nil` for a binary file
}

// Reads the files at `pickedPaths` (position in `nodeIDs` -> path) with one query.
//
// Returns:
//   - Position -> the file. `nil` where GraphQL couldn't give the repository or the file is gone
//   - An error if the query failed, as for `FetchReadmesByNodeIDs`
func readReadmeFiles(ctx context.Context, cfg *EnvConfig, nodeIDs []string, pickedPaths map[int]string) (map[int]*githubFile, error) {
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
	err := callGraphQL(ctx, cfg, contentQuery, variables, &contents)
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
