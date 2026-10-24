// Integration tests for the DB repository: finding and opening it (state, format version, guard), and writing to it (`core.UpdateDBRepoFiles` and the archive writes on top of it)
package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that the DB repository state check (`core.FindDBRepo`) tells three cases apart by the repository's content.
//   - An empty repository with no commits: `DBRepoEmpty` (the setup tool can initialize it)
//   - A repository with the marker `.starkive`: `DBRepoStarkive`
//   - A repository without the marker, even with only a README: `DBRepoForeign` (neither the setup tool nor the server writes to it)
//
// In every case, the found repository has the name that GitHub gives for its ID (`me/archive`).
func TestDBRepoState(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		prepare func(gh *fakeGitHub)
		want    core.DBRepoState
	}{
		{"empty", func(gh *fakeGitHub) { gh.files = map[string]string{}; gh.repoEmpty = true }, core.DBRepoEmpty},
		{"set up for starkive", func(gh *fakeGitHub) {}, core.DBRepoStarkive},
		{"another repository", func(gh *fakeGitHub) { gh.files = map[string]string{"README.md": "# my project\n"} }, core.DBRepoForeign},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := setupWithoutGitHubApp(t, "", "")
			c.prepare(gh)
			cfg, err := core.LoadEnvConfig()
			if err != nil {
				t.Fatal(err)
			}
			if repo, err := core.FindDBRepo(ctx, cfg); err != nil || repo.State() != c.want || repo.FullName() != "me/archive" {
				t.Errorf("got %+v %v", repo, err)
			}
		})
	}
}

// Tests that every path stops with 409 and writes nothing in a repository whose marker has a different major format version than this code (`core.DBRepoFormat`).
// It checks a marker with another major version, a marker without a version, and a marker that can't be read.
// The paths checked are opening the DB repository (`core.OpenDBRepoFromEnv`), starting sign-in, a signed-in API and the scheduled sync.
// "Wrote nothing" means there were no commits and no requests to turn the workflow on or off.
//
// NOTE: This guards the check that keeps this code from misreading or writing to a repository made by another version of starkive.
// It stops.
// It doesn't adapt or convert the format.
func TestOtherFormatStopsEverything(t *testing.T) {
	ctx := context.Background()
	for _, marker := range []string{`{"format":{"major":99,"minor":0}}` + "\n", `{}` + "\n", "not json\n"} {
		gh := setup(t, sampleStarItems, sampleStarLists)
		cookie := login(t)
		// The session cookie comes from above, and the test runs on a new fake GitHub.
		// The fake server's address is part of the cache key,
		// so the server's cached "starkive repository" result for the previous repository isn't used for the new one
		gh = setup(t, sampleStarItems, sampleStarLists)
		gh.files[core.MarkerDBPath] = marker
		if _, err := core.OpenDBRepoFromEnv(ctx); err == nil || !strings.Contains(err.Error(), "this version of starkive uses format") {
			t.Errorf("%q: open %v", marker, err)
		}
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"login":          call(api.HandleLoginSession, "POST", "/api/login-session", "", ""),
			"list":           call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie),
			"scheduled sync": callScheduledSync(),
		} {
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "format") {
				t.Errorf("%q %s: %d %s", marker, name, rec.Code, rec.Body)
			}
		}
		gh.mu.Lock()
		if len(gh.commits) != 0 || gh.workflowToggles != 0 {
			t.Errorf("%q: wrote to the repository: %q", marker, gh.commits)
		}
		gh.mu.Unlock()
	}
}

// Tests that a repository whose marker has the same major format version but another minor version is used as usual.
// A minor version only marks a change that keeps the contract, so it must not stop anything.
func TestOtherMinorFormatWorks(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.files[core.MarkerDBPath] = fmt.Sprintf(`{"format":{"major":%d,"minor":99}}`+"\n", core.DBRepoFormat.Major)
	if _, err := core.OpenDBRepoFromEnv(ctx); err != nil {
		t.Fatalf("open: %v", err)
	}
	if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", login(t)); rec.Code != 200 {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
}

// Tests that `core.UpdateDBRepoFiles` doesn't commit and stops with an error,
// when there are files to write or delete but the commit message isn't valid.
// That is an empty message, a message with no summary or only spaces after the prefix, and a message with no prefix.
// With nothing to write or delete, an empty message is fine and nothing happens (there is no commit, so the message isn't used).
func TestInvalidCommitMessageIsRefused(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	before := len(gh.commits)
	for name, message := range map[string]core.CommitMessage{
		"empty message":           {},
		"only a prefix":           core.NewCommitMessage("user(star-lists)", ""),
		"spaces after the prefix": core.NewCommitMessage("user(star-lists)", " "),
		"no prefix":               core.NewCommitMessage("", "delete Graphics"),
	} {
		err := core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
			return core.FileChangesCommit{FilesToWrite: map[string][]byte{core.StarListsDBPath: []byte("[]\n")}, Message: message}, nil
		})
		if err == nil || len(gh.commits) != before {
			t.Errorf("files with %s: %v, commits %d -> %d", name, err, before, len(gh.commits))
		}
	}
	err := core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
		return core.FileChangesCommit{FilesToDelete: []string{core.StarListsDBPath}}, nil
	})
	if err == nil || len(gh.commits) != before {
		t.Errorf("delete with empty message: %v, commits %d -> %d", err, before, len(gh.commits))
	}
	err = core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
		return core.FileChangesCommit{}, nil
	})
	if err != nil || len(gh.commits) != before {
		t.Errorf("nothing to write: %v, commits %d -> %d", err, before, len(gh.commits))
	}
}

// Tests that `core.UpdateDBRepoFiles` stops with an error and doesn't commit, when a path to read, write or delete isn't valid.
// A path to read that isn't valid stops it before `buildFileChangesCommit` is called.
// A path that is changed twice in one commit (written and deleted, or deleted twice) is refused before any tree is made.
func TestInvalidPathIsRefused(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	before := len(gh.commits)
	message := core.NewCommitMessage("user(star-lists)", "delete Graphics")
	for _, p := range []string{"/" + core.StarListsDBPath, "./" + core.StarListsDBPath} {
		called := false
		err := core.UpdateDBRepoFiles(ctx, repo, []string{p}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
			called = true
			return core.FileChangesCommit{}, nil
		})
		if err == nil || called {
			t.Errorf("read %q: %v, buildFileChangesCommit called %v", p, err, called)
		}
		err = core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
			return core.FileChangesCommit{FilesToWrite: map[string][]byte{p: []byte("[]\n")}, Message: message}, nil
		})
		if err == nil || len(gh.commits) != before {
			t.Errorf("write %q: %v, commits %d -> %d", p, err, before, len(gh.commits))
		}
		err = core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
			return core.FileChangesCommit{FilesToDelete: []string{p}, Message: message}, nil
		})
		if err == nil || len(gh.commits) != before {
			t.Errorf("delete %q: %v, commits %d -> %d", p, err, before, len(gh.commits))
		}
	}

	treeCallsBefore := gh.treeCalls
	for name, changes := range map[string]core.FileChangesCommit{
		"written and deleted": {
			FilesToWrite:  map[string][]byte{core.StarListsDBPath: []byte("[]\n")},
			FilesToDelete: []string{core.StarListsDBPath},
		},
		"deleted twice": {FilesToDelete: []string{core.StarListsDBPath, core.StarListsDBPath}},
	} {
		changes.Message = message
		err := core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
			return changes, nil
		})
		if err == nil || len(gh.commits) != before || gh.treeCalls != treeCallsBefore {
			t.Errorf("%s: %v, commits %d -> %d, tree calls %d -> %d", name, err, before, len(gh.commits), treeCallsBefore, gh.treeCalls)
		}
	}
}

// Tests that `core.UpdateDBRepoFiles` writes and deletes files in one commit.
// Deleting a file that isn't there fails the whole commit, so nothing changes (measured on GitHub, imitated by the fake).
func TestWriteAndDeleteInOneCommit(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	const notesPath = "db/notes.txt" // A file that only this test makes and deletes
	gh.files[notesPath] = "old notes\n"
	before := len(gh.commits)
	message := core.NewCommitMessage("user(star-lists)", "delete Graphics")
	writeListsAndDeleteNotes := func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
		return core.FileChangesCommit{
			FilesToWrite:  map[string][]byte{core.StarListsDBPath: []byte("[]\n")},
			FilesToDelete: []string{notesPath},
			Message:       message,
		}, nil
	}

	err := core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, writeListsAndDeleteNotes)
	_, notesLeft := gh.files[notesPath]
	if err != nil || len(gh.commits) != before+1 || notesLeft || gh.readFile(core.StarListsDBPath) != "[]\n" {
		t.Fatalf("write and delete: %v, commits %d -> %d, notes left %v, star lists %q", err, before, len(gh.commits), notesLeft, gh.readFile(core.StarListsDBPath))
	}

	// The notes file is gone now, so GitHub refuses the tree with 422 and the write in the same commit doesn't happen either
	gh.files[core.StarListsDBPath] = sampleStarLists
	treeCallsBefore := gh.treeCalls
	err = core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, writeListsAndDeleteNotes)
	var githubErr *core.GitHubAPIError
	refusedByGitHub := errors.As(err, &githubErr) && githubErr.Status == http.StatusUnprocessableEntity
	if !refusedByGitHub || gh.treeCalls != treeCallsBefore+1 || len(gh.commits) != before+1 || gh.readFile(core.StarListsDBPath) != sampleStarLists {
		t.Errorf("delete a missing file: %v, tree calls %d -> %d, commits %d -> %d", err, treeCallsBefore, gh.treeCalls, before+1, len(gh.commits))
	}
}

// Tests that a write to the config file doesn't treat it as changed when only its layout differs and the values are the same.
// It checks the compact test config file, and the same values with the keys in another order.
//   - Recording that sync is on when it is already on makes no commit and leaves the file as it is
//   - A write that changes a value commits, in the layout the server writes
func TestReformattedConfigFileIsNotRewritten(t *testing.T) {
	ctx := context.Background()
	for _, layout := range []string{"compact", "keys in another order"} {
		gh := setup(t, sampleStarItems, sampleStarLists)
		if layout == "keys in another order" {
			var values map[string]any
			if err := json.Unmarshal([]byte(gh.readFile(core.ConfigDBPath)), &values); err != nil {
				t.Fatal(err)
			}
			reordered, err := json.Marshal(values) // A map is written with its keys sorted, unlike the field order the server writes
			if err != nil {
				t.Fatal(err)
			}
			gh.files[core.ConfigDBPath] = string(reordered) + "\n"
		}
		repo := openTestDBRepo(t)
		original := gh.readFile(core.ConfigDBPath)
		before := len(gh.commits)
		if err := core.RecordSyncEnabled(ctx, repo); err != nil {
			t.Fatal(err)
		}
		if len(gh.commits) != before || gh.readFile(core.ConfigDBPath) != original {
			t.Errorf("%s, same values: commits %q", layout, gh.commits[before:])
		}
		if err := core.SaveDashboardURL(ctx, repo, "https://other.example.com"); err != nil {
			t.Fatal(err)
		}
		if len(gh.commits) != before+1 || !strings.Contains(gh.readFile(core.ConfigDBPath), "\n  \"dashboard_url\": \"https://other.example.com\"") {
			t.Errorf("%s, changed value: commits %q, file %s", layout, gh.commits[before:], gh.readFile(core.ConfigDBPath))
		}
	}
}

// Tests that `core.UpdateStarCatalog` doesn't treat a data file as changed when only its layout differs and the data is the same.
// (For example, a person edited the file and added indentation.)
//   - A write that changes nothing (with an empty message, like a sync with no new Stars) makes no error and no commit
//   - A write that changes another file commits only that file, and leaves the file with the different layout as it is
func TestReformattedArchiveFileIsNotRewritten(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	var items []map[string]any
	if err := json.Unmarshal([]byte(gh.readFile(core.StarItemsDBPath)), &items); err != nil {
		t.Fatal(err)
	}
	reformatted, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	gh.files[core.StarItemsDBPath] = string(reformatted)
	before := len(gh.commits)
	gh.mu.Unlock()
	repo := openTestDBRepo(t)

	if err := core.UpdateStarCatalog(ctx, repo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) { return core.CommitMessage{}, nil }); err != nil || len(gh.commits) != before {
		t.Errorf("nothing changed: %v, commits %d -> %d", err, before, len(gh.commits))
	}
	err = core.UpdateStarCatalog(ctx, repo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) {
		tx.StarLists = append(tx.StarLists, core.StarList{ID: 9999, Name: "new"})
		return core.NewCommitMessage("user(star-lists)", "add new"), nil
	})
	if err != nil || len(gh.commits) != before+1 || gh.readFile(core.StarItemsDBPath) != string(reformatted) || !strings.Contains(gh.readFile(core.StarListsDBPath), `"new"`) {
		t.Errorf("star lists changed: %v, commits %d -> %d", err, before, len(gh.commits))
	}
}

// Tests that each archive write reads only the files it uses.
// A broken file that a write doesn't read doesn't stop it, and is left as it is.
//   - `core.UpdateStarCatalog` doesn't read the metadata snapshots
//   - `core.UpdateStarLists` reads only the star lists
//   - `core.UpdateRepoMetaSnapshots` doesn't read the star lists, and writes only the snapshots
func TestArchiveWritesReadOnlyTheirFiles(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	setFile := func(path string, content string) {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		gh.files[path] = content
	}

	setFile(core.RepoMetaDBPath, "not JSON")
	err := core.UpdateStarCatalog(ctx, repo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) {
		tx.StarItems[0].Note = "x"
		return core.NewCommitMessage("user(star-items)", "edit note of a/one"), nil
	})
	if err != nil || gh.readFile(core.RepoMetaDBPath) != "not JSON" || !strings.Contains(gh.readFile(core.StarItemsDBPath), `"note":"x"`) {
		t.Errorf("star catalog: %v, files %q", err, gh.files)
	}

	setFile(core.StarItemsDBPath, "not JSON")
	err = core.UpdateStarLists(ctx, repo, func(starLists *[]core.StarList) (core.CommitMessage, error) {
		*starLists = append(*starLists, core.StarList{ID: 9999, Name: "new"})
		return core.NewCommitMessage("user(star-lists)", "create new"), nil
	})
	if err != nil || gh.readFile(core.StarItemsDBPath) != "not JSON" || !strings.Contains(gh.readFile(core.StarListsDBPath), `"new"`) {
		t.Errorf("star lists: %v, files %q", err, gh.files)
	}

	setFile(core.StarItemsDBPath, sampleStarItems)
	setFile(core.RepoMetaDBPath, "[]\n")
	setFile(core.StarListsDBPath, "not JSON")
	committed, err := core.UpdateRepoMetaSnapshots(ctx, repo, func(starItems []core.StarItem, repoMeta *[]core.RepoMeta) (core.CommitMessage, error) {
		starItems[0].Note = "not written"
		*repoMeta = []core.RepoMeta{{ID: starItems[0].ID, Name: starItems[0].Name, Topics: []string{}}}
		return core.NewCommitMessage("sync(repo-meta)", "refresh 1 repo, 0 unavailable"), nil
	})
	if err != nil || len(committed) != 1 || gh.readFile(core.StarListsDBPath) != "not JSON" || gh.readFile(core.StarItemsDBPath) != sampleStarItems ||
		!strings.Contains(gh.readFile(core.RepoMetaDBPath), `"id":1`) {
		t.Errorf("metadata snapshots: %v, %v, files %q", err, committed, gh.files)
	}
}

// Tests that `core.UpdateDBRepoFiles` makes no commit when its changes leave the tree as it was.
// GitHub then answers the new tree with the sha of the base tree, so no commit is made on top of it
func TestUnchangedTreeIsNotCommitted(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	before := len(gh.commits)
	err := core.UpdateDBRepoFiles(ctx, repo, []string{core.StarListsDBPath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
		return core.FileChangesCommit{
			FilesToWrite: map[string][]byte{core.StarListsDBPath: filesAtCommit.FileContent(core.StarListsDBPath)}, // The same content again
			Message:      core.NewCommitMessage("user(star-lists)", "edit Graphics"),
		}, nil
	})
	if err != nil || len(gh.commits) != before || gh.treeCalls != 1 {
		t.Errorf("%v, commits %d -> %d, tree calls %d", err, before, len(gh.commits), gh.treeCalls)
	}
}

// Tests that the server opens the DB repository only through its guard (`core.OpenDBRepo`).
// `core.FindDBRepo` returns a repository in any state, so it may be called only by `OpenDBRepo` in `core` (and by the setup tool, which isn't checked here).
// The API code (`api`) must not call it at all.
// Otherwise a new handler could write to an empty or foreign repository
func TestServerOpensDBRepoOnlyThroughTheGuard(t *testing.T) {
	sourcePaths := []string{}
	for _, pattern := range []string{"../core/*.go", "../api/*.go"} {
		paths, _ := filepath.Glob(pattern)
		sourcePaths = append(sourcePaths, paths...)
	}
	if len(sourcePaths) == 0 {
		t.Fatal("no source files found")
	}
	for _, path := range sourcePaths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			function, isFunction := decl.(*ast.FuncDecl)
			if !isFunction || function.Body == nil || function.Name.Name == "OpenDBRepo" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				var name string // Name of the called function: `FindDBRepo` in `core`, `core.FindDBRepo` in `api`
				switch called := node.(type) {
				case *ast.Ident:
					name = called.Name
				case *ast.SelectorExpr:
					name = called.Sel.Name
				}
				if name == "FindDBRepo" {
					t.Errorf("%s: %s calls FindDBRepo, which skips the guard. Use OpenDBRepo", fileSet.Position(node.Pos()), function.Name.Name)
					return false
				}
				return true
			})
		}
	}
}
