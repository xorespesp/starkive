package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that the DB repository state check (`core.ResolveDBRepo`) tells three cases apart by the repository's content.
//   - An empty repository with no commits: `DBRepoEmpty` (the setup tool can initialize it)
//   - A repository with the marker `.starkive`: `DBRepoStarkive`
//   - A repository without the marker, even with only a README: `DBRepoForeign` (neither the setup tool nor the server writes to it)
//
// In every case, the repository name is filled in with the name found by its ID (`me/archive`).
func TestDBRepoState(t *testing.T) {
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
			if state, err := core.ResolveDBRepo(cfg); err != nil || state != c.want || cfg.DBRepoFullName != "me/archive" {
				t.Errorf("got %v %v (%s)", state, err, cfg.DBRepoFullName)
			}
		})
	}
}

// Tests that every path stops with 409 and writes nothing in a repository whose marker has a different major format version than this code (`core.DBRepoFormat`).
// It checks a marker with another major version, a marker without a version, and a marker that can't be read.
// The paths checked are reading the config (`core.LoadConfig`), starting sign-in, a signed-in API and the scheduled sync.
// "Wrote nothing" means there were no commits and no requests to turn the workflow on or off.
//
// NOTE: This guards the check that keeps this code from misreading or writing to a repository made by another version of starkive.
// It stops. It doesn't adapt or convert the format.
func TestOtherFormatStopsEverything(t *testing.T) {
	for _, marker := range []string{`{"format":{"major":99,"minor":0}}` + "\n", `{}` + "\n", "not json\n"} {
		gh := setup(t, sampleStarItems, sampleStarLists)
		cookie := login(t)
		// The session cookie comes from above, and the test runs on a new fake GitHub.
		// The fake server's address is part of the cache key,
		// so the server's cached "starkive repository" result for the previous repository isn't used for the new one
		gh = setup(t, sampleStarItems, sampleStarLists)
		gh.files[core.MarkerDBPath] = marker
		if _, err := core.LoadConfig(); err == nil || !strings.Contains(err.Error(), "this version of starkive uses format") {
			t.Errorf("%q: config %v", marker, err)
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
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.files[core.MarkerDBPath] = fmt.Sprintf(`{"format":{"major":%d,"minor":99}}`+"\n", core.DBRepoFormat.Major)
	if _, err := core.LoadConfig(); err != nil {
		t.Fatalf("config: %v", err)
	}
	if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", login(t)); rec.Code != 200 {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
}

// Tests that `core.UpdateFiles` doesn't commit and stops with an error when there are files to write but the commit message is empty.
// With no files to write, an empty message is fine and nothing happens (there is no commit, so the message isn't used).
func TestEmptyCommitMessageIsRefused(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cfg := loadTestConfig(t)
	before := len(gh.commits)
	err := core.UpdateFiles(cfg, []string{core.StarListsDBPath}, func(original map[string][]byte) (map[string][]byte, core.CommitMessage, error) {
		return map[string][]byte{core.StarListsDBPath: []byte("[]\n")}, core.CommitMessage{}, nil
	})
	if err == nil || len(gh.commits) != before {
		t.Errorf("files without a message: %v, commits %d -> %d", err, before, len(gh.commits))
	}
	err = core.UpdateFiles(cfg, []string{core.StarListsDBPath}, func(original map[string][]byte) (map[string][]byte, core.CommitMessage, error) {
		return nil, core.CommitMessage{}, nil
	})
	if err != nil || len(gh.commits) != before {
		t.Errorf("nothing to write: %v, commits %d -> %d", err, before, len(gh.commits))
	}
}

// Tests that `core.UpdateArchive` doesn't treat a data file as changed when only its layout differs and the data is the same.
// (For example, a person edited the file and added indentation.)
//   - A write that changes nothing (with an empty message, like a sync with no new Stars) makes no error and no commit
//   - A write that changes another file commits only that file, and leaves the file with the different layout as it is
func TestReformattedArchiveFileIsNotRewritten(t *testing.T) {
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
	cfg := loadTestConfig(t)

	if _, err := core.UpdateArchive(cfg, func(a *core.Archive) (core.CommitMessage, error) { return core.CommitMessage{}, nil }); err != nil || len(gh.commits) != before {
		t.Errorf("nothing changed: %v, commits %d -> %d", err, before, len(gh.commits))
	}
	_, err = core.UpdateArchive(cfg, func(a *core.Archive) (core.CommitMessage, error) {
		a.StarLists = append(a.StarLists, core.StarList{ID: 9999, Name: "new"})
		return core.NewCommitMessage("user(star-lists)", "add new"), nil
	})
	if err != nil || len(gh.commits) != before+1 || gh.readFile(core.StarItemsDBPath) != string(reformatted) || !strings.Contains(gh.readFile(core.StarListsDBPath), `"new"`) {
		t.Errorf("star lists changed: %v, commits %d -> %d", err, before, len(gh.commits))
	}
}
