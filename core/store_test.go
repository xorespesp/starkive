// Unit tests for the storage engine (`store.go`)
package core

import "testing"

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
