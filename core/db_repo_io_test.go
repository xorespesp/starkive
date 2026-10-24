// Unit tests for the storage engine of the DB repository files (`db_repo_io.go`)
package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Starts a test server that answers the tree API of the DB repository `me/archive` at commit `c1`, like GitHub.
//   - `db/readmes/45`: a README, a marker, a file that isn't a README, and a file in a subfolder
//   - `db/huge`: a list that GitHub cut short (`truncated`)
//   - Any other folder: 404
//
// Returns:
//   - The DB repository `me/archive` whose GitHub requests go to the server (`newTestDBRepo`), and the number of requests it got.
//     The server answers in its own goroutine, so the count is an atomic counter, not a plain `int`
func startFakeTreeAPI(t *testing.T) (*DBRepo, *atomic.Int32) {
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("recursive") != "1" {
			http.Error(w, `{"message":"recursive must be 1"}`, http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/repos/me/archive/git/trees/c1:db/readmes/45":
			fmt.Fprint(w, `{"tree":[`+
				`{"path":"12345.md","type":"blob","sha":"a"},`+
				`{"path":"345.no-readme","type":"blob","sha":"b"},`+
				`{"path":"notes.txt","type":"blob","sha":"c"},`+
				`{"path":"sub","type":"tree","sha":"d"},`+
				`{"path":"sub/12345.md","type":"blob","sha":"e"}],"truncated":false}`)
		case "/repos/me/archive/git/trees/c1:db/huge":
			fmt.Fprint(w, `{"tree":[{"path":"1.md","type":"blob","sha":"a"}],"truncated":true}`)
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close) // `t.Cleanup`: runs when the test ends, like a destructor
	return newTestDBRepo(server.URL), requests
}

// Makes the starkive DB repository `me/archive` (branch `main`) whose GitHub requests go to `apiURL`, without asking GitHub to find it
func newTestDBRepo(apiURL string) *DBRepo {
	return &DBRepo{cfg: &EnvConfig{GitHubAPI: apiURL, DBRepoAccessToken: "test-token"}, fullName: "me/archive", branch: "main", state: DBRepoStarkive}
}

// Tests how `readBlobSHAsInFolder` reads a folder listing.
//   - Only files are kept, with the folder path put in front.
//     Files in subfolders are included
//   - A folder that doesn't exist gives an empty map, not an error
//   - A list that GitHub cut short is an error (502)
//   - A folder path that isn't clean is refused before any request
func TestReadBlobSHAsInFolder(t *testing.T) {
	ctx := context.Background()
	repo, requests := startFakeTreeAPI(t)

	got, err := readBlobSHAsInFolder(ctx, repo, "c1", "db/readmes/45")
	want := map[string]string{
		"db/readmes/45/12345.md":      "a",
		"db/readmes/45/345.no-readme": "b",
		"db/readmes/45/notes.txt":     "c",
		"db/readmes/45/sub/12345.md":  "e",
	}
	if err != nil || !maps.Equal(got, want) {
		t.Errorf("folder: %v, %v", got, err)
	}

	if got, err := readBlobSHAsInFolder(ctx, repo, "c1", "db/missing"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("missing folder: %v, %v", got, err)
	}

	var httpErr *HTTPError
	if _, err := readBlobSHAsInFolder(ctx, repo, "c1", "db/huge"); !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway {
		t.Errorf("cut short list: %v", err)
	}

	requestsBefore := requests.Load()
	if _, err := readBlobSHAsInFolder(ctx, repo, "c1", "db/readmes/"); err == nil || requests.Load() != requestsBefore {
		t.Errorf("folder path not clean: %v, requests %d -> %d", err, requestsBefore, requests.Load())
	}
}

// Tests which commits `FileChangesCommit.check` accepts.
//   - A commit that changes nothing is refused, even with a valid message
//   - A commit with changes needs a valid message (`errInvalidCommitMessage`)
//   - A path that isn't clean, or that is changed twice, is refused
func TestFileChangesCommitCheck(t *testing.T) {
	message := NewCommitMessage("user(star-lists)", "delete Graphics")
	newLists := map[string][]byte{StarListsDBPath: []byte("[]\n")}
	cases := []struct {
		name    string
		changes FileChangesCommit
		valid   bool
	}{
		{"write", FileChangesCommit{FilesToWrite: newLists, Message: message}, true},
		{"delete", FileChangesCommit{FilesToDelete: []string{RepoMetaDBPath}, Message: message}, true},
		{"empty", FileChangesCommit{Message: message}, false},
		{"no message", FileChangesCommit{FilesToWrite: newLists}, false},
		{"path not clean", FileChangesCommit{FilesToDelete: []string{"./" + RepoMetaDBPath}, Message: message}, false},
		{"written and deleted", FileChangesCommit{FilesToWrite: newLists, FilesToDelete: []string{StarListsDBPath}, Message: message}, false},
		{"deleted twice", FileChangesCommit{FilesToDelete: []string{RepoMetaDBPath, RepoMetaDBPath}, Message: message}, false},
	}
	for _, c := range cases {
		if err := c.changes.check(); (err == nil) != c.valid {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := (FileChangesCommit{FilesToWrite: newLists}).check(); !errors.Is(err, errInvalidCommitMessage) {
		t.Errorf("no message: got %v", err)
	}
	if !(FileChangesCommit{}).isEmpty() || (FileChangesCommit{FilesToDelete: []string{RepoMetaDBPath}}).isEmpty() {
		t.Errorf("isEmpty is wrong")
	}
}

// Tests that only a clean path relative to the repository root is valid.
func TestIsValidRepoPath(t *testing.T) {
	cases := map[string]bool{
		"config.json":                         true,
		"db/star-items.json":                  true,
		".github/workflows/starkive-sync.yml": true,
		"db/.gitkeep":                         true,
		"":                                    false,
		".":                                   false,
		"..":                                  false,
		"/config.json":                        false,
		"./config.json":                       false,
		"../config.json":                      false,
		"db/../config.json":                   false,
		"db//star-items.json":                 false,
		"db/./star-items.json":                false,
		"db/":                                 false,
		`db\star-items.json`:                  false,
	}
	for p, want := range cases {
		if got := isValidRepoPath(p); got != want {
			t.Errorf("%q: got %v", p, got)
		}
	}
}

// Tests that `computeGitBlobSHA` gives the same value as git (`git hash-object`), for an empty file and a short one
func TestComputeGitBlobSHA(t *testing.T) {
	if got := computeGitBlobSHA(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("empty: %s", got)
	}
	if got := computeGitBlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("hello: %s", got)
	}
}

// Tests that a file whose body stops partway is a 502, like an answer that never came (`readDBRepoFileAtRef`).
// The server promises more bytes than it sends, so the connection closes in the middle of the body
func TestReadDBRepoFileCutInTheBodyIsBadGateway(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "partial")
	}))
	t.Cleanup(server.Close)
	if content, err := ReadDBRepoFile(ctx, newTestDBRepo(server.URL), StarItemsDBPath); content != nil || !isBadGateway(err) {
		t.Errorf("body cut: %q, %v", content, err)
	}
}

// Tests that a REST answer that isn't the expected JSON is a 502, like any GitHub failure (`callDBRepoAPI`, `DecodeGitHubResponse`).
// A branch tip answer with no commit, or with a commit but no tree, is the same
func TestUnreadableRESTAnswerIsBadGateway(t *testing.T) {
	ctx := context.Background()
	for _, answer := range []string{"<html>Unicorn!</html>", "[]", "[{}]", `[{"sha":"c1"}]`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, answer)
		}))
		t.Cleanup(server.Close)
		if answer == "<html>Unicorn!</html>" {
			if _, err := readBranchCommit(ctx, newTestDBRepo(server.URL)); !isBadGateway(err) {
				t.Errorf("branch read, %s: %v", answer, err)
			}
		}
		if _, err := readBranchHead(ctx, newTestDBRepo(server.URL)); !isBadGateway(err) {
			t.Errorf("branch tip read, %s: %v", answer, err)
		}
	}
}
