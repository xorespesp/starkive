// Integration tests for the README snapshots (`core/readme.go`) through the API, against the fake GitHub
package test

import (
	"strings"
	"testing"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that removing an item deletes its README snapshots in the same commit, and leaves every other file.
//   - All files of the item go: its README and a marker left next to it
//   - Files of other items in the same folder, files of other folders, and a file that isn't a snapshot stay
//   - Only a write that removes an item lists the folder
//   - Editing a note sends no listing request
//   - An item with no snapshot is removed as usual (the folder listing finds nothing)
func TestRemovingAnItemDeletesItsReadmeSnapshots(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	itemReadme := core.ReadmesDBDir + "/01/1.md"
	itemMarker := core.ReadmesDBDir + "/01/1.no-readme"
	kept := []string{
		core.ReadmesDBDir + "/01/101.md",    // Another ID in the same folder
		core.ReadmesDBDir + "/01/notes.txt", // Not a README snapshot
		core.ReadmesDBDir + "/02/2.md",      // Item 2, in another folder
	}
	for _, path := range append([]string{itemReadme, itemMarker}, kept...) {
		gh.files[path] = "# " + path + "\n"
	}
	commitsBefore := len(gh.commits)

	if rec := call(api.HandleArchiveStarItems, "DELETE", "/api/archive-star-items?id=1", "", cookie); rec.Code != 200 {
		t.Fatalf("remove item 1: %d %s", rec.Code, rec.Body)
	}
	_, readmeLeft := gh.files[itemReadme]
	_, markerLeft := gh.files[itemMarker]
	if readmeLeft || markerLeft || len(gh.commits) != commitsBefore+1 || gh.treeListCalls != 1 {
		t.Errorf("remove item 1: README left %v, marker left %v, commits %d -> %d, listings %d", readmeLeft, markerLeft, commitsBefore, len(gh.commits), gh.treeListCalls)
	}
	for _, path := range kept {
		if _, ok := gh.files[path]; !ok {
			t.Errorf("%s was deleted", path)
		}
	}

	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"x"}`, cookie); rec.Code != 200 || gh.treeListCalls != 1 {
		t.Errorf("edit a note: %d %s, listings %d", rec.Code, rec.Body, gh.treeListCalls)
	}

	delete(gh.files, core.ReadmesDBDir+"/02/2.md")
	rec := call(api.HandleArchiveStarItems, "DELETE", "/api/archive-star-items?id=2", "", cookie)
	if rec.Code != 200 || strings.Contains(gh.readFile(core.StarItemsDBPath), `"id":2,`) || gh.treeListCalls != 2 {
		t.Errorf("remove item 2 with no snapshot: %d %s, listings %d", rec.Code, rec.Body, gh.treeListCalls)
	}
}

// Tests that the README snapshots to delete are found again after another commit gets in.
// Another device deletes the README of item 1 right before the first branch move.
// The retry lists the folder at the new commit, so it doesn't ask GitHub to delete a file that is gone (GitHub refuses the whole tree with 422, measured)
func TestReadmeSnapshotsAreListedAgainAfterAConflict(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	itemReadme := core.ReadmesDBDir + "/01/1.md"
	gh.files[itemReadme] = "# one\n"
	gh.interfereOnce = func(files map[string]string) {
		delete(files, itemReadme)
	}

	rec := call(api.HandleArchiveStarItems, "DELETE", "/api/archive-star-items?id=1", "", cookie)
	if _, readmeLeft := gh.files[itemReadme]; rec.Code != 200 || readmeLeft || gh.treeListCalls != 2 {
		t.Errorf("remove item 1: %d %s, README left %v, listings %d", rec.Code, rec.Body, readmeLeft, gh.treeListCalls)
	}
}
