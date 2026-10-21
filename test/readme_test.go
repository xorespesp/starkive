// Integration tests for the README snapshots (`core/readme.go`) through the API, against the fake GitHub
package test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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

// Tests that Add saves the README of the new repository in the same commit.
//   - A README is saved as `<ID>.<extension>`, and old files of that ID (another extension, a marker) are deleted
//   - A repository with no README gets the marker
//   - A README that can't be fetched doesn't stop Add. The item is added with no README file, and no folder is listed
func TestAddSavesTheReadme(t *testing.T) {
	const linuxID = 2325298 // ID that the fake GitHub gives `torvalds/linux`
	folder := core.ReadmesDBDir + "/98/"
	cases := []struct {
		name          string
		readme        *fakeReadme       // README on the fake GitHub. `nil` if it has none
		savedBefore   map[string]string // Files of the ID already in the DB repository: path -> content
		wantFiles     map[string]string // Files of the ID after Add: path -> content
		wantListCalls int               // Folder listings expected
	}{
		{"README", &fakeReadme{name: "README.md", content: "# Linux\n"},
			map[string]string{folder + "2325298.rst": "old\n", folder + "2325298.no-readme": ""},
			map[string]string{folder + "2325298.md": "# Linux\n"}, 1},
		{"no README", nil, nil, map[string]string{folder + "2325298.no-readme": ""}, 1},
		{"README request fails", &fakeReadme{status: http.StatusInternalServerError}, nil, map[string]string{}, 0},
	}
	for _, c := range cases {
		gh := setup(t, sampleStarItems, sampleStarLists)
		cookie := login(t)
		if c.readme != nil {
			gh.readmes[linuxID] = *c.readme
		}
		for path, content := range c.savedBefore {
			gh.files[path] = content
		}
		commitsBefore := len(gh.commits)

		rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"torvalds/linux"}`, cookie)
		if rec.Code != 200 || len(gh.commits) != commitsBefore+1 || gh.readmeCalls != 1 || gh.treeListCalls != c.wantListCalls {
			t.Errorf("%s: %d %s, commits %d -> %d, README requests %d, listings %d",
				c.name, rec.Code, rec.Body, commitsBefore, len(gh.commits), gh.readmeCalls, gh.treeListCalls)
		}
		files := map[string]string{}
		for path, content := range gh.files {
			if strings.HasPrefix(path, folder) {
				files[path] = content
			}
		}
		if fmt.Sprint(files) != fmt.Sprint(c.wantFiles) {
			t.Errorf("%s: files %q, want %q", c.name, files, c.wantFiles)
		}
	}
}

// Tests that a README given to `core.Archive.AddFetchedReadmeToSave` for an item that isn't in the archive is not saved.
// Such a file would have no item to own it, and nothing would ever delete it
func TestReadmeOfAnItemNotInTheArchiveIsNotSaved(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cfg := loadTestConfig(t)
	gh.readmes[999] = fakeReadme{name: "README.md", content: "# Not archived\n"}
	fetched, err := core.FetchReadmeByRepoID(cfg, 999, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	_, err = core.UpdateArchive(cfg, func(a *core.Archive) (core.CommitMessage, error) {
		a.AddFetchedReadmeToSave(999, fetched)
		a.StarItems[0].Note = "x" // A change to commit, so the commit isn't skipped as empty
		return core.NewCommitMessage("user(star-items)", "edit note of a/one"), nil
	})
	if _, saved := gh.files[core.ReadmesDBDir+"/99/999.md"]; err != nil || saved || gh.treeListCalls != 0 {
		t.Errorf("save README of item 999: %v, saved %v, listings %d", err, saved, gh.treeListCalls)
	}
}

// Tests reading README snapshots through `GET /api/archive-repo-meta?id=`.
//   - A saved README gives `saved`, its extension as `format`, and its text without the cut line, with `truncated`
//   - A file with no extension has no `format`
//   - A README and a marker together give the README
//   - Only the marker gives `absent`, and no file gives `unchecked`
//   - An `id` that isn't a positive number is 400, and without `id` the raw metadata file comes back as before
func TestReadingReadmeSnapshotsThroughAPI(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	const cutLine = "[starkive: the rest of this README was not saved]\n"
	gh.files[core.ReadmesDBDir+"/01/1.rst"] = "# One\n" + cutLine // Sorted after `1.no-readme`, so the README must win on purpose
	gh.files[core.ReadmesDBDir+"/01/1.no-readme"] = ""
	gh.files[core.ReadmesDBDir+"/02/2"] = "Two\n"
	gh.files[core.ReadmesDBDir+"/03/3.no-readme"] = ""

	cases := []struct {
		id   string
		want string // Answer body, or the status code for a failure
	}{
		{"1", `{"id":1,"readme":{"state":"saved","format":"rst","text":"# One\n","truncated":true}}`},
		{"2", `{"id":2,"readme":{"state":"saved","text":"Two\n"}}`},
		{"3", `{"id":3,"readme":{"state":"absent"}}`},
		{"4", `{"id":4,"readme":{"state":"unchecked"}}`},
		{"0", "400"},
		{"-1", "400"},
		{"x", "400"},
		{"", "400"}, // `id` sent with no value
	}
	for _, c := range cases {
		rec := call(api.HandleArchiveRepoMeta, "GET", "/api/archive-repo-meta?id="+c.id, "", cookie)
		got := strings.TrimSpace(rec.Body.String())
		if rec.Code != 200 {
			got = fmt.Sprint(rec.Code)
		}
		if got != c.want {
			t.Errorf("id %s: got %s, want %s", c.id, got, c.want)
		}
	}

	if rec := call(api.HandleArchiveRepoMeta, "GET", "/api/archive-repo-meta", "", cookie); rec.Code != 200 || rec.Body.String() != "[]" {
		t.Errorf("raw metadata file: %d %s", rec.Code, rec.Body)
	}
}

// Returns a time on sync day `dayNumber` (days since 1970-01-01, each starting at 06:00 UTC), to pick today's README group
func syncDay(dayNumber int) time.Time {
	return time.Unix(0, 0).UTC().Add(6*time.Hour + time.Duration(dayNumber)*24*time.Hour + time.Hour)
}

// Tests which READMEs the README sync step (`core.SyncReadmeSnapshotsWithGitHub`) checks, and what it writes.
//   - An item with no snapshot is always checked
//   - An item with a snapshot is checked only on the day of its group (`ID % 61`)
//   - A changed README is written again, and a README under a new extension replaces the old file
//   - Items in `skippedIDs` aren't asked
//   - Everything checked goes in one `sync(readmes)` commit that lists the saved READMEs
func TestSyncReadmeSnapshotsWithGitHubChecksTheRightItems(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cfg := loadTestConfig(t)
	gh.files[core.ReadmesDBDir+"/01/1.txt"] = "Old\n" // Saved under another extension than the new README
	gh.readmes[1] = fakeReadme{name: "README.md", content: "# New\n"}
	gh.readmes[2] = fakeReadme{name: "README.rst", content: "Two\n"}

	// Day 2: item 1 has a snapshot and isn't in group 2, so only item 2 (no snapshot) is checked
	checked, err := core.SyncReadmeSnapshotsWithGitHub(cfg, testDeadline(), nil, syncDay(2))
	if err != nil || checked != 1 || gh.readFile(core.ReadmesDBDir+"/01/1.txt") != "Old\n" || gh.readFile(core.ReadmesDBDir+"/02/2.rst") != "Two\n" {
		t.Errorf("day 2: %d %v, files %q", checked, err, gh.files)
	}

	// Day 1: item 1 is in group 1, so its changed README is written. Item 2 now has a snapshot and isn't in group 1
	commitsBefore := len(gh.commits)
	checked, err = core.SyncReadmeSnapshotsWithGitHub(cfg, testDeadline(), nil, syncDay(1))
	_, oldFileLeft := gh.files[core.ReadmesDBDir+"/01/1.txt"]
	if err != nil || checked != 1 || gh.readFile(core.ReadmesDBDir+"/01/1.md") != "# New\n" || oldFileLeft || len(gh.commits) != commitsBefore+1 ||
		gh.commits[len(gh.commits)-1] != "sync(readmes): update readmes of 1 repo\n\nSaved:\n- a/one" {
		t.Errorf("day 1: %d %v, commits %q", checked, err, gh.commits[commitsBefore:])
	}

	// A skipped item isn't asked, even with no snapshot
	delete(gh.files, core.ReadmesDBDir+"/02/2.rst")
	readmeCallsBefore := gh.readmeCalls
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(cfg, testDeadline(), []int64{2}, syncDay(2)); err != nil || checked != 0 || gh.readmeCalls != readmeCallsBefore {
		t.Errorf("skipped item: %d %v, README requests %d -> %d", checked, err, readmeCallsBefore, gh.readmeCalls)
	}
}

// Tests that the README sync step deletes the snapshots of items no longer in the archive.
// With nothing to check, it still makes a commit for them, with a subject that says so
func TestSyncReadmeSnapshotsWithGitHubDeletesSnapshotsOfRemovedItems(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cfg := loadTestConfig(t)
	gh.files[core.ReadmesDBDir+"/01/1.no-readme"] = ""
	gh.files[core.ReadmesDBDir+"/02/2.no-readme"] = ""
	gh.files[core.ReadmesDBDir+"/03/3.md"] = "# Removed item\n" // Item 3 isn't in the archive

	checked, err := core.SyncReadmeSnapshotsWithGitHub(cfg, testDeadline(), nil, syncDay(3)) // Group 3: neither item 1 nor item 2
	_, orphanLeft := gh.files[core.ReadmesDBDir+"/03/3.md"]
	if err != nil || checked != 0 || orphanLeft || gh.commits[len(gh.commits)-1] != "sync(readmes): remove readmes of 1 repo no longer in the archive" {
		t.Errorf("%d %v, orphan left %v, commits %q", checked, err, orphanLeft, gh.commits)
	}
}

// Tests the limits of the README sync step.
//   - More than `maxReadmesPerCommit` (300) items go in several commits
//   - A rate limit answer stops it early without an error, and the READMEs fetched before it are committed
//   - With too little time before the deadline, it sends no README request
func TestSyncReadmeSnapshotsWithGitHubLimits(t *testing.T) {
	manyItems := []string{}
	for id := 1; id <= 305; id++ {
		manyItems = append(manyItems, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	gh := setup(t, "[\n"+strings.Join(manyItems, ",\n")+"\n]\n", sampleStarLists)
	cfg := loadTestConfig(t)
	commitsBefore := len(gh.commits)
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(cfg, testDeadline(), nil, syncDay(0)); err != nil || checked != 305 || len(gh.commits) != commitsBefore+2 {
		t.Errorf("305 items: %d %v, commits %d -> %d", checked, err, commitsBefore, len(gh.commits))
	}

	gh = setup(t, "[\n"+strings.Join(manyItems, ",\n")+"\n]\n", sampleStarLists)
	cfg = loadTestConfig(t)
	gh.readmeRateLimitFrom = 11 // The first 10 README requests are answered, then GitHub answers 429
	checked, err := core.SyncReadmeSnapshotsWithGitHub(cfg, testDeadline(), nil, syncDay(0))
	savedFiles := 0
	for path := range gh.files {
		if strings.HasPrefix(path, core.ReadmesDBDir+"/") {
			savedFiles++
		}
	}
	// The fake GitHub answers one request at a time, and each of the 6 goroutines stops after its first 429
	if err != nil || checked != 10 || savedFiles != 10 || gh.readmeCalls > 10+6 {
		t.Errorf("rate limit: %d %v, files %d, README requests %d", checked, err, savedFiles, gh.readmeCalls)
	}

	gh = setup(t, sampleStarItems, sampleStarLists)
	cfg = loadTestConfig(t)
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(cfg, time.Now().Add(30*time.Second), nil, syncDay(0)); err != nil || checked != 0 || gh.readmeCalls != 0 {
		t.Errorf("no time: %d %v, README requests %d", checked, err, gh.readmeCalls)
	}
}
