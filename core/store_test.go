// Unit tests for the storage engine (`store.go`)
package core

import (
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
//   - A config that sends GitHub requests to the server, and the number of requests it got.
//     The server answers in its own goroutine, so the count is an atomic counter, not a plain `int`
func startFakeTreeAPI(t *testing.T) (*Config, *atomic.Int32) {
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
	return &Config{GitHubAPI: server.URL, DBRepoFullName: "me/archive", DBRepoAccessToken: "test-token"}, requests
}

// Tests how `readBlobSHAsInFolder` reads a folder listing.
//   - Only files are kept, with the folder path put in front. Files in subfolders are included
//   - A folder that doesn't exist gives an empty map, not an error
//   - A list that GitHub cut short is an error (502)
//   - A folder path that isn't clean is refused before any request
func TestReadBlobSHAsInFolder(t *testing.T) {
	cfg, requests := startFakeTreeAPI(t)

	got, err := readBlobSHAsInFolder(cfg, "c1", "db/readmes/45")
	want := map[string]string{
		"db/readmes/45/12345.md":      "a",
		"db/readmes/45/345.no-readme": "b",
		"db/readmes/45/notes.txt":     "c",
		"db/readmes/45/sub/12345.md":  "e",
	}
	if err != nil || !maps.Equal(got, want) {
		t.Errorf("folder: %v, %v", got, err)
	}

	if got, err := readBlobSHAsInFolder(cfg, "c1", "db/missing"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("missing folder: %v, %v", got, err)
	}

	var httpErr *HTTPError
	if _, err := readBlobSHAsInFolder(cfg, "c1", "db/huge"); !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadGateway {
		t.Errorf("cut short list: %v", err)
	}

	requestsBefore := requests.Load()
	if _, err := readBlobSHAsInFolder(cfg, "c1", "db/readmes/"); err == nil || requests.Load() != requestsBefore {
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

// Tests that `gitBlobSHA` gives the same value as git (`git hash-object`), for an empty file and a short one
func TestGitBlobSHA(t *testing.T) {
	if got := gitBlobSHA(nil); got != "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391" {
		t.Errorf("empty: %s", got)
	}
	if got := gitBlobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Errorf("hello: %s", got)
	}
}
