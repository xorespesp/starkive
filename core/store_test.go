// Unit tests for the storage engine (`store.go`)
package core

import (
	"errors"
	"testing"
)

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
