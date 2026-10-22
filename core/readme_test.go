// Unit tests for the README snapshot files (`readme.go`)
package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests how the README file content is made and read back.
//   - Short text is kept as it is. A UTF-8 byte order mark is removed
//   - Blank text isn't usable
//   - Long text is cut after its last line break within `maxReadmeBytes`,
//     or at a character boundary if there is no line break or it would keep less than half of it, and the cut line is added
//   - Text over the limit only by blank space at its end loses that space and isn't cut
//   - Text of exactly `maxReadmeBytes` isn't cut
func TestBuildReadmeSnapshotFile(t *testing.T) {
	cases := []struct {
		name       string
		readmeText string
		want       string // File content. Empty if not usable
	}{
		{"short", "# Title\nbody\n", "# Title\nbody\n"},
		{"UTF-8 byte order mark", "\xEF\xBB\xBF# Title\n", "# Title\n"},
		{"blank", " \n\t\n", ""},
		{"exactly the limit", strings.Repeat("a", maxReadmeBytes), strings.Repeat("a", maxReadmeBytes)},
		{"cut at a line break", strings.Repeat(strings.Repeat("x", 99)+"\n", 100),
			strings.Repeat(strings.Repeat("x", 99)+"\n", 81) + readmeCutLine + "\n"},
		{"cut at a character boundary", strings.Repeat("가", 3000), strings.Repeat("가", 2730) + "\n" + readmeCutLine + "\n"},
		{"line break at the start", "\n" + strings.Repeat("x", 9000), "\n" + strings.Repeat("x", maxReadmeBytes-1) + "\n" + readmeCutLine + "\n"},
		{"short first line, then one long line", "# Title\n" + strings.Repeat("y", 50000),
			"# Title\n" + strings.Repeat("y", maxReadmeBytes-8) + "\n" + readmeCutLine + "\n"},
		{"over the limit only by blank space at the end", strings.Repeat("a", 8000) + strings.Repeat(" \n", 500), strings.Repeat("a", 8000)},
	}
	for _, c := range cases {
		got, usable := buildReadmeSnapshotFile(c.readmeText)
		if usable != (c.want != "") || string(got) != c.want {
			t.Errorf("%s: got %q (usable %v)", c.name, got, usable)
		}
	}

	// Reading back gives the text without the cut line, and whether it was cut
	content, _ := buildReadmeSnapshotFile(strings.Repeat(strings.Repeat("x", 99)+"\n", 100))
	if text, isCut := parseReadmeSnapshotFile(content); !isCut || text != strings.Repeat(strings.Repeat("x", 99)+"\n", 81) {
		t.Errorf("parse cut file: cut %v, %d bytes", isCut, len(text))
	}
	if text, isCut := parseReadmeSnapshotFile([]byte("# Title\n")); isCut || text != "# Title\n" {
		t.Errorf("parse whole file: cut %v, %q", isCut, text)
	}
}

// Tests which paths under `ReadmesDBDir` count as README snapshots.
// Only `<ID>`, `<ID>.<extension>` and `<ID>.no-readme` in the folder of that ID count.
// A wrong folder, a leading zero, a disallowed extension and a deeper path are ignored
func TestCollectReadmeSnapshotsByID(t *testing.T) {
	blobSHAByPath := map[string]string{
		"db/readmes/45/12345":           "a",
		"db/readmes/45/12345.no-readme": "b",
		"db/readmes/45/345.md":          "c", // Another item in the same folder
		"db/readmes/46/12345.md":        "d", // Wrong folder
		"db/readmes/45/012345.md":       "e", // Leading zero
		"db/readmes/45/12345.Md":        "f", // Upper case extension, which `readmeExtensionPattern` doesn't allow
		"db/readmes/45/x/12345.md":      "g", // Deeper path
		"db/readmes/45/notes.txt":       "h",
	}
	got := []string{}
	for id, snapshots := range collectReadmeSnapshotsByID(blobSHAByPath) {
		for _, snapshot := range snapshots {
			got = append(got, fmt.Sprintf("%d %s %s %v", id, snapshot.path, snapshot.blobSHA, snapshot.isAbsent))
		}
	}
	slices.Sort(got)
	want := []string{
		"12345 db/readmes/45/12345 a false",
		"12345 db/readmes/45/12345.no-readme b true",
		"345 db/readmes/45/345.md c false",
	}
	if !slices.Equal(got, want) {
		t.Errorf("grouped = %q", got)
	}
}

// Tests that every path `FetchedReadme.buildPath` makes is a clean path, and that `collectReadmeSnapshotsByID` reads it back under the same ID.
// IDs 7 and 105 are checked too, so the zero-padded folder names (`07`, `05`) are covered
func TestFetchedReadmePath(t *testing.T) {
	fetchedReadmes := map[string]FetchedReadme{ // Path for ID 12345 -> fetched README
		"db/readmes/45/12345.md":        {extension: "md", content: []byte("# Title\n")},
		"db/readmes/45/12345":           {content: []byte("# Title\n")},
		"db/readmes/45/12345.no-readme": {isAbsent: true},
	}
	for wantPath, fetched := range fetchedReadmes {
		for _, id := range []int64{12345, 7, 105} {
			path := fetched.buildPath(id)
			if id == 12345 && path != wantPath {
				t.Errorf("path for %d: got %q, want %q", id, path, wantPath)
			}
			snapshots := collectReadmeSnapshotsByID(map[string]string{path: "sha"})[id]
			if !isValidRepoPath(path) || len(snapshots) != 1 || snapshots[0].isAbsent != fetched.isAbsent {
				t.Errorf("%q isn't read back as a snapshot of %d: %v", path, id, snapshots)
			}
		}
	}
}

// Tests that `readReadmeSnapshotsInFolder` keeps only the README snapshots of a folder listing.
// The file that isn't a README and the file in a subfolder of the listing are left out
func TestReadReadmeSnapshotsInFolder(t *testing.T) {
	ctx := context.Background()
	cfg, _ := startFakeTreeAPI(t)
	snapshotsByID, err := readReadmeSnapshotsInFolder(ctx, cfg, "c1", "db/readmes/45")
	want := map[int64][]readmeSnapshot{
		12345: {{path: "db/readmes/45/12345.md", blobSHA: "a"}},
		345:   {{path: "db/readmes/45/345.no-readme", blobSHA: "b", isAbsent: true}},
	}
	if err != nil || fmt.Sprint(snapshotsByID) != fmt.Sprint(want) {
		t.Errorf("snapshots: %v, %v", snapshotsByID, err)
	}
}

// Tests that `readReadmeSnapshotPaths` lists the folder of the item once, and returns only the paths of that item.
//   - Item 12345 shares its folder with item 345 and other files, and gets only its own README
//   - An item whose folder doesn't exist gets no path
func TestReadReadmeSnapshotPaths(t *testing.T) {
	ctx := context.Background()
	cfg, requests := startFakeTreeAPI(t)

	paths, err := readReadmeSnapshotPaths(ctx, cfg, "c1", 12345)
	if want := []string{"db/readmes/45/12345.md"}; err != nil || !slices.Equal(paths, want) || requests.Load() != 1 {
		t.Errorf("item 12345: %q, %v, requests %d", paths, err, requests.Load())
	}

	if paths, err := readReadmeSnapshotPaths(ctx, cfg, "c1", 7); err != nil || len(paths) != 0 || requests.Load() != 2 {
		t.Errorf("item with no folder: %q, %v, requests %d", paths, err, requests.Load())
	}
}

// Tests which files of a folder count as READMEs, and the order GitHub picks among them.
//   - `readme` alone or with one extension counts, in any case. Two dots, other names, folders, submodules and symbolic links don't
//   - Markdown comes before another extension, which comes before no extension. A tie goes by byte order
//   - A folder that doesn't exist has none
func TestListReadmeFileNames(t *testing.T) {
	file := func(name string) githubFolderEntry {
		return githubFolderEntry{Name: name, Type: "blob", Mode: 0o100644}
	}
	cases := []struct {
		name    string
		entries []githubFolderEntry
		want    []string
	}{
		{"one", []githubFolderEntry{file("LICENSE"), file("Readme.md")}, []string{"Readme.md"}},
		{"no extension", []githubFolderEntry{file("README")}, []string{"README"}},
		{"other extension before none", []githubFolderEntry{file("README"), file("README.rst")}, []string{"README.rst", "README"}},
		{"Markdown first", []githubFolderEntry{file("README.TXT"), file("README"), file("README.md")}, []string{"README.md", "README.TXT", "README"}},
		{"markdown extension", []githubFolderEntry{file("README.rst"), file("readme.markdown")}, []string{"readme.markdown", "README.rst"}},
		{"two dots", []githubFolderEntry{file("README.ko.md"), file("README.zh-CN.md")}, []string{}},
		{"two dots beside one", []githubFolderEntry{file("README.Music.md"), file("README.md")}, []string{"README.md"}},
		{"byte order", []githubFolderEntry{file("readme.md"), file("README.md")}, []string{"README.md", "readme.md"}},
		{"other names", []githubFolderEntry{file("READMEFIRST.md"), file("docs-readme.md")}, []string{}},
		{"folder, submodule, symbolic link", []githubFolderEntry{
			{Name: "readme", Type: "tree", Mode: 0o40000},
			{Name: "README.md", Type: "commit", Mode: 0o160000},
			{Name: "README.rst", Type: "blob", Mode: 0o120000},
		}, []string{}},
	}
	for _, c := range cases {
		if got := (&githubFolder{Entries: c.entries}).listReadmeFileNames(); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	var missingFolder *githubFolder
	if got := missingFolder.listReadmeFileNames(); len(got) != 0 {
		t.Errorf("no folder: %q", got)
	}
}

// Tests finding a folder by its name in any case.
// Among several, the first in byte order wins, as on GitHub (`Docs` before `docs`, `doCs` before `docS`, measured)
func TestFindSubfolderName(t *testing.T) {
	folder := func(names ...string) *githubFolder {
		entries := []githubFolderEntry{{Name: "DOCS", Type: "blob", Mode: 0o100644}} // A file with the name isn't a folder
		for _, name := range names {
			entries = append(entries, githubFolderEntry{Name: name, Type: "tree", Mode: 0o40000})
		}
		return &githubFolder{Entries: entries}
	}
	cases := []struct {
		folder *githubFolder
		want   string
	}{
		{folder("docs"), "docs"},
		{folder("Docs"), "Docs"},
		{folder("docs", "Docs"), "Docs"},
		{folder("docS", "doCs"), "doCs"},
		{folder("documents", "src"), ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := c.folder.findSubfolderName(docsFolderName); got != c.want {
			t.Errorf("%v: %q, want %q", c.folder, got, c.want)
		}
	}
}

// One repository on the GraphQL test server of `TestFetchReadmesByNodeIDs`
type testReadmeRepo struct {
	github     []string          // Files in `.github`. `nil` if the folder doesn't exist
	githubName string            // Name of the `.github` folder, if it isn't exactly `.github` (e.g. `.GitHub`)
	root       []string          // Files in the root folder. `nil` for an empty repository
	docs       []string          // Files in `docs`. `nil` if the folder doesn't exist
	docsName   string            // Name of the `docs` folder, if it isn't exactly `docs` (e.g. `Docs`)
	texts      map[string]string // File path -> text. A file not in it is binary (`text` is `null`)
	missing    bool              // `true` if GraphQL answers `null` with `NOT_FOUND`
	other      bool              // `true` if the ID isn't a repository (GraphQL answers `{}`)
	denied     bool              // `true` if the content query answers `null` with a `FORBIDDEN` error, while the rest of the answer is usable
	rootFails  bool              // `true` if the listing answers the root folder as `null` with an error inside the repository
	docsFails  bool              // `true` if the `docs` listing answers the folder as `null` with an error inside the repository
	docsGone   bool              // `true` if the `docs` listing answers the repository as `null`, as for one gone after the first query
	fileGone   bool              // `true` if the content query answers the file as `null`, as for a file gone after the listing
	links      []string          // Files in the root folder that are symbolic links
}

// Tests fetching READMEs of many repositories with GraphQL.
//   - The README is picked from `.github`, then the root, then `docs`, and only repositories with none in the first two get `docs` listed.
//     The root listing tells whether a folder exists and its case (`.GitHub`, `Docs`), and a folder not named exactly is listed by its own name
//   - The picked file is read by its path, cut like any README, and its extension is kept in lower case
//   - A repository not found, one with no README anywhere, and a blank README are absent
//   - A binary README isn't a README, as on GitHub: the next one in the same folder is read,
//     or one in the next folder (listing `docs` if needed), and with none left the README is absent.
//     A repository whose last README is found binary by the last content query is absent.
//     A repository with a pick still unread after `maxReadmeContentQueries` content queries gives `nil`.
//     If a later query fails, the READMEs already read are kept and the repositories still left give `nil`.
//     A repository whose next folder listing is cut (as at the deadline of `ctx`) gives `nil` too, not absent
//   - An ID that isn't a repository, a README whose content query alone failed,
//     a repository with an error inside it (a folder that failed), a repository gone before a later folder listing,
//     and a file gone before the content query give `nil`, so they are asked again later.
//     The test server answers like GitHub: only the fields asked for, each folder by the path in its expression,
//     each file by its alias and its own path variable, and an error for a variable that isn't declared with the right type
//   - A rate limit passes through as an error that wraps `errGitHubRateLimited` (each kind is tested in `TestCallGraphQL`).
//     A failed query, an answer with no data and an answer whose node count doesn't match are errors that don't wrap it.
//     The whole call fails even when only a later folder listing or the content query fails
//   - A call whose deadline has passed fails at once, and a query still waiting at the deadline fails then.
//     Both fail with a 502 that wraps `context.DeadlineExceeded`
func TestFetchReadmesByNodeIDs(t *testing.T) {
	ctx := context.Background()
	longReadme := strings.Repeat(strings.Repeat("y", 99)+"\n", 100) // 10KB
	repos := map[string]testReadmeRepo{
		"R_md":       {root: []string{"LICENSE", "README.md"}, texts: map[string]string{"README.md": "# MD\n"}},
		"R_github":   {github: []string{"README.md"}, root: []string{"README.md"}, texts: map[string]string{".github/README.md": "# GitHub\n", "README.md": "# Root\n"}},
		"R_docs":     {root: []string{"main.c"}, docs: []string{"README.rst"}, texts: map[string]string{"docs/README.rst": "Docs\n"}},
		"R_none":     {root: []string{"main.c"}, docs: []string{"guide.md"}},
		"R_empty":    {},
		"R_gone":     {missing: true},
		"U_user":     {other: true},
		"R_binary":   {root: []string{"README.md"}},
		"R_blank":    {root: []string{"README.md"}, texts: map[string]string{"README.md": " \n"}},
		"R_noext":    {root: []string{"README"}, texts: map[string]string{"README": "plain\n"}},
		"R_badext":   {root: []string{"README.old-md"}, texts: map[string]string{"README.old-md": "old\n"}},
		"R_long":     {root: []string{"README.md"}, texts: map[string]string{"README.md": longReadme}},
		"R_several":  {root: []string{"README", "README.TXT", "readme.md", "README.md"}, texts: map[string]string{"README.md": "# Upper\n"}},
		"R_denied":   {root: []string{"README.md"}, texts: map[string]string{"README.md": "# Denied\n"}, denied: true},
		"R_rootErr":  {root: []string{"README.md"}, rootFails: true},
		"R_docsErr":  {root: []string{"main.c"}, docs: []string{"README.md"}, docsFails: true},
		"R_docsGone": {root: []string{"main.c"}, docsGone: true},
		"R_fileGone": {root: []string{"README.md"}, texts: map[string]string{"README.md": "# Gone\n"}, fileGone: true},
		"R_link":     {root: []string{"main.c"}, links: []string{"README.md"}, texts: map[string]string{"README.md": "docs/intro.md"}},
		"R_upper":    {root: []string{"README.MD"}, texts: map[string]string{"README.MD": "# Upper case\n"}},
		// Binary READMEs: a file not in `texts` is binary
		"R_binNext":   {root: []string{"README.md", "README.txt"}, texts: map[string]string{"README.txt": "Text next\n"}},
		"R_binGitHub": {github: []string{"README.md"}, root: []string{"README.md"}, texts: map[string]string{"README.md": "# Root after binary\n"}},
		"R_binDocs":   {root: []string{"README.md"}, docs: []string{"README.rst"}, texts: map[string]string{"docs/README.rst": "Docs after binary\n"}},
		"R_binThree":  {root: []string{"README.md", "README.txt"}, docs: []string{"README.md"}},
		"R_binMany":   {root: []string{"README.md", "README.rst", "README.txt", "README"}},
		// Folders in another case
		"R_upperDocs":   {root: []string{"main.c"}, docs: []string{"README.md"}, docsName: "Docs", texts: map[string]string{"Docs/README.md": "# Upper Docs\n"}},
		"R_upperGitHub": {github: []string{"README.md"}, githubName: ".GitHub", root: []string{"README.md"}, texts: map[string]string{".GitHub/README.md": "# Upper GitHub\n", "README.md": "# Root\n"}},
	}
	// Repositories with nothing to read come between the ones with a README, so a position that slips would show
	ids := []string{"R_md", "R_github", "R_docs", "R_none", "R_empty", "R_gone", "U_user", "R_binary", "R_blank", "R_noext", "R_badext", "R_long", "R_several", "R_denied", "R_rootErr", "R_docsErr", "R_docsGone", "R_fileGone", "R_link", "R_upper", "R_binNext", "R_binGitHub", "R_binDocs", "R_binThree", "R_binMany", "R_upperDocs", "R_upperGitHub"}

	var mu sync.Mutex
	var failWith func(w http.ResponseWriter) // If not `nil`, every query is answered with it
	failQuery := ""                          // Kind of the query that `failWith` answers. Empty for every kind
	failContentFrom := 0                     // If not 0, content queries from this one on (counting from 1) fail with 502
	var cutLaterFolders context.CancelFunc   // If not `nil`, a folder query after a content query calls it and waits with no answer, as at the deadline
	queries := []string{}                    // Kind of each query received: `listing`, `folders` or `content`
	foldersAsked := []string{}               // `<ID>:<folder path>` of each folder listed after the first query
	var query string                         // The query being answered. Only the fields it asks for are given, as GitHub does
	asks := func(name string) bool { return regexp.MustCompile(`[{\s]` + name + `[\s}]`).MatchString(query) }
	folder := func(names []string, links []string, folders []string) any {
		if names == nil {
			return nil
		}
		entries := []map[string]any{}
		addEntry := func(name string, mode int) {
			entry := map[string]any{}
			if asks("name") {
				entry["name"] = name
			}
			if asks("type") {
				entry["type"] = "blob"
				if mode == 0o40000 {
					entry["type"] = "tree"
				}
			}
			if asks("mode") {
				entry["mode"] = mode
			}
			entries = append(entries, entry)
		}
		for _, name := range names {
			addEntry(name, 0o100644)
		}
		for _, name := range links {
			addEntry(name, 0o120000)
		}
		for _, name := range folders {
			addEntry(name, 0o40000)
		}
		return map[string]any{"entries": entries}
	}
	// One folder field of a listing query: its alias and the folder path in its expression (`HEAD:<path>`, empty for the root)
	folderFields := regexp.MustCompile(`(\w+): object\(expression: "HEAD:([^"]*)"\)`)
	// Names of the `.github` and `docs` folders of `repo` in the root. Empty if the folder doesn't exist
	folderNames := func(repo testReadmeRepo) (string, string) {
		githubName, docsName := "", ""
		if repo.github != nil {
			githubName = cmp.Or(repo.githubName, ".github")
		}
		if repo.docs != nil || repo.docsFails || repo.docsGone {
			docsName = cmp.Or(repo.docsName, "docs")
		}
		return githubName, docsName
	}
	// Answers repository `id` at position `i` of a listing query, with one field for each folder the query asks for
	listedRepository := func(i int, id string, errs *[]map[string]any) map[string]any {
		repo := repos[id]
		fields := map[string]any{}
		for _, field := range folderFields.FindAllStringSubmatch(query, -1) {
			alias, folderPath := field[1], field[2]
			githubName, docsName := folderNames(repo)
			if repo.rootFails && folderPath == "" {
				fields[alias] = nil
				*errs = append(*errs, map[string]any{"type": "FORBIDDEN", "path": []any{"nodes", i, alias}, "message": "Something went wrong"})
				continue
			}
			switch folderPath {
			case "":
				fields[alias] = folder(repo.root, repo.links, slices.DeleteFunc([]string{githubName, docsName}, func(name string) bool { return name == "" }))
			case githubName:
				fields[alias] = folder(repo.github, nil, nil)
			default: // A path in another case is another folder in git
				fields[alias] = nil
			}
		}
		if asks("id") {
			fields["id"] = id
		}
		return fields
	}
	// One field of a folder or content query: its alias, the variable of the `node_id`, and the variable of the path
	contentFields := regexp.MustCompile(`(\w+): node\(id: \$(\w+)\) \{ \.\.\. on Repository \{ object\(expression: \$(\w+)\)`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		query = body.Query
		kind := "content"
		switch {
		case strings.Contains(body.Query, `"HEAD:.github"`):
			kind = "listing"
		case strings.Contains(body.Query, "... on Tree"):
			kind = "folders"
		}
		if failWith != nil && (failQuery == "" || failQuery == kind) {
			failWith(w)
			return
		}
		if kind == "content" && failContentFrom != 0 && countOf(queries, "content")+1 >= failContentFrom {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
			return
		}
		if kind == "folders" && cutLaterFolders != nil && countOf(queries, "content") > 0 {
			cutLaterFolders()
			select { // The caller gives up when its `ctx` ends, which closes the connection
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		if !isEveryGraphQLVariableDeclared(query) {
			fmt.Fprint(w, `{"data":null,"errors":[{"message":"Variable is not declared, is declared with another type, or is declared but not used"}]}`)
			return
		}
		data := map[string]any{}
		errs := []map[string]any{}
		nodeIDs, _ := body.Variables["ids"].([]any)
		switch kind {
		case "listing":
			queries = append(queries, "listing")
			nodes := []any{}
			for i, id := range nodeIDs {
				repo := repos[id.(string)]
				switch {
				case repo.missing:
					nodes = append(nodes, nil)
					errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{"nodes", i}, "message": "Could not resolve to a node"})
				case repo.other:
					nodes = append(nodes, map[string]any{})
				default:
					nodes = append(nodes, listedRepository(i, id.(string), &errs))
				}
			}
			data["nodes"] = nodes
		case "folders":
			queries = append(queries, "folders")
			for _, field := range contentFields.FindAllStringSubmatch(query, -1) {
				alias, idVariable, pathVariable := field[1], field[2], field[3]
				nodeID, _ := body.Variables[idVariable].(string)
				folderPath, _ := body.Variables[pathVariable].(string)
				folderPath = strings.TrimPrefix(folderPath, "HEAD:")
				foldersAsked = append(foldersAsked, nodeID+":"+folderPath)
				repo := repos[nodeID]
				githubName, docsName := folderNames(repo)
				switch {
				case repo.docsGone:
					data[alias] = nil
				case repo.docsFails && folderPath == docsName:
					data[alias] = map[string]any{"object": nil}
					errs = append(errs, map[string]any{"type": "FORBIDDEN", "path": []any{alias, "object"}, "message": "Something went wrong"})
				case folderPath == docsName:
					data[alias] = map[string]any{"object": folder(repo.docs, nil, nil)}
				case folderPath == githubName:
					data[alias] = map[string]any{"object": folder(repo.github, nil, nil)}
				default:
					data[alias] = map[string]any{"object": nil}
				}
			}
		default:
			queries = append(queries, "content")
			for _, field := range contentFields.FindAllStringSubmatch(query, -1) {
				alias, idVariable, pathVariable := field[1], field[2], field[3]
				nodeID, idGiven := body.Variables[idVariable].(string)
				path, pathGiven := body.Variables[pathVariable].(string)
				if !idGiven || !pathGiven || !strings.HasPrefix(path, "HEAD:") {
					fmt.Fprint(w, `{"data":null,"errors":[{"message":"Variable is used but not given"}]}`)
					return
				}
				repo := repos[nodeID]
				switch {
				case repo.denied:
					data[alias] = nil
					errs = append(errs, map[string]any{"type": "FORBIDDEN", "path": []any{alias}, "message": "Resource not accessible"})
				case repo.fileGone:
					data[alias] = map[string]any{"object": nil}
				default:
					file := map[string]any{}
					t, isText := repo.texts[strings.TrimPrefix(path, "HEAD:")]
					if asks("isBinary") {
						file["isBinary"] = !isText
					}
					if asks("text") {
						var text any // `nil` (JSON `null`) for a binary file
						if isText {
							text = t
						}
						file["text"] = text
					}
					data[alias] = map[string]any{"object": file}
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "errors": errs})
	}))
	t.Cleanup(server.Close)
	cfg := &Config{GitHubAPI: server.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}

	got, err := FetchReadmesByNodeIDs(ctx, cfg, ids)
	wantLong, _ := buildReadmeSnapshotFile(longReadme)
	absent := &FetchedReadme{isAbsent: true}
	want := []*FetchedReadme{
		{extension: "md", content: []byte("# MD\n")},
		{extension: "md", content: []byte("# GitHub\n")},
		{extension: "rst", content: []byte("Docs\n")},
		absent, absent, absent,
		nil,
		absent, // The only README is binary
		absent,
		{content: []byte("plain\n")},
		{content: []byte("old\n")}, // `old-md` isn't allowed, so it has no extension
		{extension: "md", content: wantLong},
		{extension: "md", content: []byte("# Upper\n")},
		nil, nil, nil, nil, nil,
		absent, // A symbolic link isn't picked, and nothing else is a README
		{extension: "md", content: []byte("# Upper case\n")},
		{extension: "txt", content: []byte("Text next\n")},
		{extension: "md", content: []byte("# Root after binary\n")},
		{extension: "rst", content: []byte("Docs after binary\n")},
		absent, // The third content query reads its last README, which is binary
		nil,    // A README is still unread after `maxReadmeContentQueries` content queries
		{extension: "md", content: []byte("# Upper Docs\n")},
		{extension: "md", content: []byte("# Upper GitHub\n")}, // `.GitHub` comes before the root, as `.github` does
	}
	if err != nil || fmt.Sprint(derefReadmes(got)) != fmt.Sprint(derefReadmes(want)) {
		t.Errorf("READMEs: %v\n got %v", err, derefReadmes(got))
	}
	// Only folders the root lists are listed later: `docs` when the first two folders have no README left, and `.GitHub` by its own name.
	// A later round lists `docs` for repositories whose first two folders had only binary READMEs, and reads the next picks
	slices.Sort(foldersAsked) // A query lists its folders in no fixed order
	if !slices.Equal(queries, []string{"listing", "folders", "content", "folders", "content", "folders", "content"}) ||
		!slices.Equal(foldersAsked, []string{"R_binDocs:docs", "R_binThree:docs", "R_docs:docs", "R_docsErr:docs", "R_docsGone:docs", "R_none:docs", "R_upperDocs:Docs", "R_upperGitHub:.GitHub"}) {
		t.Errorf("queries %v, folders asked %v", queries, foldersAsked)
	}

	queries = nil
	if got, err := FetchReadmesByNodeIDs(ctx, cfg, []string{"R_gone", "U_user"}); err != nil ||
		fmt.Sprint(derefReadmes(got)) != fmt.Sprint(derefReadmes([]*FetchedReadme{absent, nil})) || !slices.Equal(queries, []string{"listing"}) {
		t.Errorf("nothing to read: %v, %v, queries %v", derefReadmes(got), err, queries)
	}

	// The second content query fails: the README read in the first is kept, and the repository still left gives `nil`
	queries, failContentFrom = nil, 2
	if got, err := FetchReadmesByNodeIDs(ctx, cfg, []string{"R_md", "R_binNext"}); err != nil ||
		fmt.Sprint(derefReadmes(got)) != fmt.Sprint(derefReadmes([]*FetchedReadme{{extension: "md", content: []byte("# MD\n")}, nil})) {
		t.Errorf("later query fails: %v, %v, queries %v", derefReadmes(got), err, queries)
	}
	failContentFrom = 0

	// The `docs` listing after a binary pick is cut, as at the deadline of `ctx`.
	// `R_binDocs` still has `docs` to look in, so it gives `nil`.
	// `R_binary` has no folder left, so it is absent without that listing
	cutCtx, cut := context.WithCancel(ctx)
	defer cut()
	queries, cutLaterFolders = nil, cut
	if got, err := FetchReadmesByNodeIDs(cutCtx, cfg, []string{"R_md", "R_binary", "R_binDocs"}); err != nil ||
		fmt.Sprint(derefReadmes(got)) != fmt.Sprint(derefReadmes([]*FetchedReadme{{extension: "md", content: []byte("# MD\n")}, absent, nil})) ||
		!slices.Equal(queries, []string{"listing", "content"}) {
		t.Errorf("later folder listing cut: %v, %v, queries %v", derefReadmes(got), err, queries)
	}
	cutLaterFolders = nil

	errorCases := []struct {
		name          string
		query         string // Kind of the query that fails. Empty for every kind
		answer        func(w http.ResponseWriter)
		isRateLimited bool // The error wraps `errGitHubRateLimited`
	}{
		{"rate limit with 200", "", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)
		}, true},
		{"rate limit with 403", "", func(w http.ResponseWriter) {
			http.Error(w, `{"message":"You have exceeded a secondary rate limit"}`, http.StatusForbidden)
		}, true},
		{"other error", "", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`)
		}, false},
		{"502 HTML", "", func(w http.ResponseWriter) {
			http.Error(w, "<html>502 Bad Gateway</html>", http.StatusBadGateway)
		}, false},
		{"node count", "", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":{"nodes":[]}}`)
		}, false},
		{"folders with no data", "folders", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`)
		}, false},
		{"content with no data", "content", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"data":null,"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`)
		}, false},
		{"rate limit at the content query", "content", func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			fmt.Fprint(w, `{"data":null,"errors":[{"message":"API rate limit exceeded"}]}`)
		}, true},
	}
	for _, c := range errorCases {
		mu.Lock()
		failWith = c.answer
		failQuery = c.query
		mu.Unlock()
		got, err := FetchReadmesByNodeIDs(ctx, cfg, []string{"R_md", "R_docs"})
		if err == nil || got != nil || errors.Is(err, errGitHubRateLimited) != c.isRateLimited {
			t.Errorf("%s: %v, %v", c.name, got, err)
		}
	}

	pastCtx, cancelPast := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelPast()
	if _, err := FetchReadmesByNodeIDs(pastCtx, cfg, []string{"R_md"}); !errors.Is(err, context.DeadlineExceeded) || !isBadGateway(err) {
		t.Errorf("deadline already passed: %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, `{"data":{"nodes":[null]}}`)
	}))
	t.Cleanup(slow.Close)
	start := time.Now()
	slowCfg := &Config{GitHubAPI: slow.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}
	shortCtx, cancelShort := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancelShort()
	if _, err := FetchReadmesByNodeIDs(shortCtx, slowCfg, []string{"R_md"}); !errors.Is(err, context.DeadlineExceeded) || !isBadGateway(err) ||
		!strings.Contains(err.Error(), "GitHub didn't answer in time") || time.Since(start) > 400*time.Millisecond {
		t.Errorf("slow answer past the deadline: %v after %v", err, time.Since(start))
	}
}

// Reports whether GraphQL query `query` declares in its head exactly the variables its body uses,
// each with the type GitHub expects (`$ids: [ID!]!`, `$id<n>: ID!`, `$path<n>: String!`).
// GitHub fails the whole query otherwise (docs).
func isEveryGraphQLVariableDeclared(query string) bool {
	head, body, _ := strings.Cut(query, "{")
	declared := map[string]string{} // Variable name -> declared type
	for _, m := range regexp.MustCompile(`\$(\w+): ([\w!\[\]]+)`).FindAllStringSubmatch(head, -1) {
		declared[m[1]] = m[2]
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$(\w+)`).FindAllStringSubmatch(body, -1) {
		used[m[1]] = true
		wantType := "String!"
		switch {
		case m[1] == "ids":
			wantType = "[ID!]!"
		case strings.HasPrefix(m[1], "id"):
			wantType = "ID!"
		}
		if declared[m[1]] != wantType {
			return false
		}
	}
	return len(used) == len(declared)
}

// Tests that a README call cut at its deadline counts as running out of time, not as a failure of GitHub.
// Counting it as a failure would stop the sync step as if GitHub were down (`errReadmesAllFailed`).
//   - A call cut at the deadline of `ctx`
//   - A call cut at `fetchEndBy`, which comes before the deadline of `ctx`.
//     Its first query answers in time, and the next one waits past `fetchEndBy`.
//     The call ends at `fetchEndBy`, before the time limit of that query, so the time kept for the commit stays free
func TestReadmeCallCutAtDeadlineIsOutOfTime(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // The server notices a closed connection only after the body is read
		select {                    // Waits until the caller gives up
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	cfg := &Config{GitHubAPI: slow.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	fetched, stopErr := fetchReadmesInParallel(ctx, cfg, []StarItem{{ID: 1, NodeID: "R_1"}}, time.Now().Add(time.Hour))
	if len(fetched) != 0 || !errors.Is(stopErr, errReadmesOutOfTime) {
		t.Errorf("cut at the deadline of ctx: %v, %v", fetched, stopErr)
	}

	var queries atomic.Int32
	listingThenSlow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if queries.Add(1) == 1 {
			time.Sleep(800 * time.Millisecond) // The next query starts after this, so its time limit ends well after `fetchEndBy`
			fmt.Fprint(w, `{"data":{"nodes":[{"id":"R_1","github":null,"root":{"entries":[{"name":"README.md","type":"blob","mode":33188}]}}]}}`)
			return
		}
		io.Copy(io.Discard, r.Body) // The server notices a closed connection only after the body is read
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(listingThenSlow.Close)
	savedClient := httpClient
	httpClient = &http.Client{Timeout: 2 * time.Second} // Stands in for `githubTimeout`, so a call can start with `fetchEndBy` close
	t.Cleanup(func() { httpClient = savedClient })
	cfg = &Config{GitHubAPI: listingThenSlow.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}
	start := time.Now()
	fetched, stopErr = fetchReadmesInParallel(context.Background(), cfg, []StarItem{{ID: 1, NodeID: "R_1"}}, start.Add(2100*time.Millisecond))
	took := time.Since(start) // Cut at 2.1 seconds. Without the cut, the second query would wait until 2.8 seconds or later
	if len(fetched) != 0 || !errors.Is(stopErr, errReadmesOutOfTime) || queries.Load() != 2 || took > 2500*time.Millisecond {
		t.Errorf("cut at fetchEndBy: %v, %v, queries %d, after %v", fetched, stopErr, queries.Load(), took)
	}
}

// Tests that a single request that hits `githubTimeout` counts as a failed call, not as running out of time.
// Both can give an error that matches `context.DeadlineExceeded`, depending on the Go version, so running out of time is told by the context.
// So this test guards only on a Go version where they match
func TestReadmeCallTimeoutIsFailure(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // The server notices a closed connection only after the body is read
		select {                    // Waits until the caller gives up
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	savedClient := httpClient
	httpClient = &http.Client{Timeout: 100 * time.Millisecond} // Stands in for `githubTimeout`, which is too long for a test
	t.Cleanup(func() { httpClient = savedClient })
	cfg := &Config{GitHubAPI: slow.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}
	fetched, stopErr := fetchReadmesInParallel(context.Background(), cfg, []StarItem{{ID: 1, NodeID: "R_1"}}, time.Now().Add(time.Hour))
	if len(fetched) != 0 || !errors.Is(stopErr, errReadmesAllFailed) {
		t.Errorf("one slow request: %v, %v", fetched, stopErr)
	}
}

// Reports whether `Respond` answers `err` with 502 (an `HTTPError` with that status, found through the wrapping)
func isBadGateway(err error) bool {
	var httpErr *HTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusBadGateway
}

// Returns how many times `value` is in `values`
func countOf(values []string, value string) int {
	count := 0
	for _, v := range values {
		if v == value {
			count++
		}
	}
	return count
}

// Returns the READMEs `readmes` as values, so they print with their content. `nil` stays `nil`
func derefReadmes(readmes []*FetchedReadme) []any {
	values := []any{}
	for _, readme := range readmes {
		if readme == nil {
			values = append(values, nil)
		} else {
			values = append(values, *readme)
		}
	}
	return values
}

// Tests what `planReadmeSnapshotUpdate` writes and deletes for one item.
//   - A new README is written and the absent marker next to it is deleted
//   - The same content at the same path writes nothing
//   - Changed content at the same path is written again, and nothing is deleted
//   - A README under another extension replaces the old file
//   - An absent README keeps a saved README, and deletes only an absent marker next to it
//   - An absent README of an item with no saved README writes the absent marker, once
func TestPlanReadmeSnapshotUpdate(t *testing.T) {
	fetched := FetchedReadme{extension: "md", content: []byte("# Title\n")}
	absent := FetchedReadme{isAbsent: true}
	savedMarkdown := readmeSnapshot{path: "db/readmes/45/12345.md", blobSHA: gitBlobSHA([]byte("# Title\n"))}
	oldMarkdown := readmeSnapshot{path: "db/readmes/45/12345.md", blobSHA: gitBlobSHA([]byte("# Old\n"))}
	savedMarker := readmeSnapshot{path: "db/readmes/45/12345.no-readme", blobSHA: gitBlobSHA(nil), isAbsent: true}
	savedText := readmeSnapshot{path: "db/readmes/45/12345.rst", blobSHA: "old"}

	cases := []struct {
		name        string
		fetched     FetchedReadme
		saved       []readmeSnapshot
		wantWritten []string
		wantDeleted []string
	}{
		{"new README", fetched, []readmeSnapshot{savedMarker}, []string{savedMarkdown.path}, []string{savedMarker.path}},
		{"same content", fetched, []readmeSnapshot{savedMarkdown}, nil, nil},
		{"changed content", fetched, []readmeSnapshot{oldMarkdown}, []string{savedMarkdown.path}, nil},
		{"other extension", fetched, []readmeSnapshot{savedText}, []string{savedMarkdown.path}, []string{savedText.path}},
		{"absent with a saved README", absent, []readmeSnapshot{savedMarkdown}, nil, nil},
		{"absent with a saved README and a marker", absent, []readmeSnapshot{savedMarkdown, savedMarker}, nil, []string{savedMarker.path}},
		{"absent with nothing saved", absent, nil, []string{savedMarker.path}, nil},
		{"absent with the marker saved", absent, []readmeSnapshot{savedMarker}, nil, nil},
	}
	for _, c := range cases {
		changes := FileChangesCommit{}
		planReadmeSnapshotUpdate(12345, c.fetched, c.saved, &changes)
		written := []string{}
		for path := range changes.FilesToWrite {
			written = append(written, path)
		}
		// `fmt.Sprint` prints a `nil` slice and an empty one the same way, so "nothing" compares equal either way
		if fmt.Sprint(written) != fmt.Sprint(c.wantWritten) || fmt.Sprint(changes.FilesToDelete) != fmt.Sprint(c.wantDeleted) {
			t.Errorf("%s: written %q, deleted %q", c.name, written, changes.FilesToDelete)
		}
	}
}
