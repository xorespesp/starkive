// Integration tests for the README snapshots (`core/archive_readmes.go`, `core/github_readmes.go`) through the API, against the fake GitHub
package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that removing an item deletes its README snapshots in the same commit, and leaves every other file.
//   - All files of the item go: its README and an absent marker left next to it
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
//   - A README is saved as `<ID>.<extension>`, and old files of that ID (another extension, an absent marker) are deleted
//   - A repository with no README gets the absent marker
//   - A README that can't be fetched doesn't stop Add. The item is added with no README file, and no folder is listed
//   - The README is asked by GraphQL (a listing query, then the content if the listings show a README)
func TestAddSavesTheReadme(t *testing.T) {
	const linuxID = 2325298 // ID that the fake GitHub gives `torvalds/linux`
	folder := core.ReadmesDBDir + "/98/"
	cases := []struct {
		name          string
		readme        *fakeReadme       // README on the fake GitHub. `nil` if it has none
		savedBefore   map[string]string // Files of the ID already in the DB repository: path -> content
		wantFiles     map[string]string // Files of the ID after Add: path -> content
		wantListCalls int               // Folder listings of the DB repository expected
		wantQueries   int               // README queries by GraphQL expected
	}{
		{"README", &fakeReadme{name: "README.md", content: "# Linux\n"},
			map[string]string{folder + "2325298.rst": "old\n", folder + "2325298.no-readme": ""},
			map[string]string{folder + "2325298.md": "# Linux\n"}, 1, 2},
		{"no README", nil, nil, map[string]string{folder + "2325298.no-readme": ""}, 1, 1}, // The root lists no `docs` folder, so the listing alone tells
		{"README can't be given now", &fakeReadme{unavailable: true}, nil, map[string]string{}, 0, 1},
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
		if rec.Code != 200 || len(gh.commits) != commitsBefore+1 || gh.readmeQueries != c.wantQueries || gh.treeListCalls != c.wantListCalls {
			t.Errorf("%s: %d %s, commits %d -> %d, README queries %d, listings %d",
				c.name, rec.Code, rec.Body, commitsBefore, len(gh.commits), gh.readmeQueries, gh.treeListCalls)
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

// Tests Add when fetching the README fails as a whole, and when another commit gets in first.
//   - When every README query fails, or GitHub answers with the rate limit, the item is still added, with no README file
//   - When another device commits first, the change is applied again, and the README is saved with it
func TestAddWithFailingOrConflictingReadme(t *testing.T) {
	const linuxID = 2325298 // ID that the fake GitHub gives `torvalds/linux`
	readmePath := core.ReadmesDBDir + "/98/2325298.md"
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.readmes[linuxID] = fakeReadme{name: "README.md", content: "# Linux\n"}
	gh.readmeQueryFails = func(queryNumber int) bool { return true }
	rec := call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"torvalds/linux"}`, login(t))
	_, saved := gh.files[readmePath]
	if rec.Code != 200 || saved || !strings.Contains(gh.readFile(core.StarItemsDBPath), `"id":2325298`) {
		t.Errorf("README queries fail: %d %s, README saved %v", rec.Code, rec.Body, saved)
	}

	gh = setup(t, sampleStarItems, sampleStarLists)
	gh.readmes[linuxID] = fakeReadme{name: "README.md", content: "# Linux\n"}
	gh.readmeRateLimitFrom = 1
	rec = call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"torvalds/linux"}`, login(t))
	_, saved = gh.files[readmePath]
	if rec.Code != 200 || saved || !strings.Contains(gh.readFile(core.StarItemsDBPath), `"id":2325298`) {
		t.Errorf("rate limit: %d %s, README saved %v", rec.Code, rec.Body, saved)
	}

	gh = setup(t, sampleStarItems, sampleStarLists)
	gh.readmes[linuxID] = fakeReadme{name: "README.md", content: "# Linux\n"}
	gh.interfereOnce = func(files map[string]string) { files["other.txt"] = "from another device\n" }
	rec = call(api.HandleArchiveStarItems, "POST", "/api/archive-star-items", `{"repo_full_name_or_url":"torvalds/linux"}`, login(t))
	if rec.Code != 200 || gh.readFile(readmePath) != "# Linux\n" || gh.readFile("other.txt") != "from another device\n" {
		t.Errorf("conflict: %d %s, README %q", rec.Code, rec.Body, gh.readFile(readmePath))
	}
}

// Tests how many GitHub requests reading one README snapshot takes.
// A saved README takes three (the branch tip, the folder listing, the file), and the other states take two
func TestReadReadmeSnapshotRequests(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.files[core.ReadmesDBDir+"/01/1.md"] = "# One\n"
	gh.files[core.ReadmesDBDir+"/02/2.no-readme"] = ""
	for _, c := range []struct {
		id    int64
		state string
		want  int // Requests expected
	}{{1, "saved", 3}, {2, "absent", 2}, {3, "unchecked", 2}} {
		before := gh.requests
		got, err := core.ReadReadmeSnapshot(ctx, repo, c.id)
		if err != nil || got.State != c.state || gh.requests-before != c.want {
			t.Errorf("item %d: %v, state %q, requests %d", c.id, err, got.State, gh.requests-before)
		}
	}
}

// Tests that a README given to `core.Archive.AddFetchedReadmeToSave` for an item that isn't in the archive is not saved.
// Such a file would have no item to own it.
// The folder of that item isn't even listed
func TestReadmeOfAnItemNotInTheArchiveIsNotSaved(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.readmes[999] = fakeReadme{name: "README.md", content: "# Not archived\n"}
	fetched, err := core.FetchReadmesByNodeIDs(ctx, repo.EnvConfig(), []string{"R_999"})
	if err != nil || fetched[0] == nil {
		t.Fatalf("fetch: %v, %v", err, fetched)
	}
	_, err = core.UpdateArchive(ctx, repo, func(a *core.Archive) (core.CommitMessage, error) {
		a.AddFetchedReadmeToSave(999, *fetched[0])
		a.StarItems[0].Note = "x" // A change to commit, so the commit isn't skipped as empty
		return core.NewCommitMessage("user(star-items)", "edit note of a/one"), nil
	})
	if _, saved := gh.files[core.ReadmesDBDir+"/99/999.md"]; err != nil || saved || gh.treeListCalls != 0 {
		t.Errorf("save README of item 999: %v, saved %v, listings %d", err, saved, gh.treeListCalls)
	}
}

// Tests reading README snapshots through `GET /api/archive-repo-meta?id=`.
//   - A saved README gives `saved`, its extension as `format`, and its text without the truncation line, with `truncated`
//   - A file with no extension has no `format`
//   - A README and an absent marker together give the README
//   - Only the absent marker gives `absent`, and no file gives `unchecked`
//   - An `id` that isn't a positive number is 400, and without `id` the raw metadata file comes back
func TestReadingReadmeSnapshotsThroughAPI(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	// Spelled out instead of `readmeTruncationLine`, because it is the format of files kept in DB repositories.
	// A change to the line must fail this test
	const truncationLine = ">>>>>>> starkive: truncated <<<<<<<\n"
	gh.files[core.ReadmesDBDir+"/01/1.rst"] = "# One\n" + truncationLine // Sorted after `1.no-readme`, so the README must win on purpose
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
//   - Items in `skippedIDs` and items without `node_id` aren't asked
//   - Everything checked goes in one `sync(readmes)` commit that lists the saved READMEs
func TestSyncReadmeSnapshotsWithGitHubChecksTheRightItems(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.files[core.ReadmesDBDir+"/01/1.txt"] = "Old\n" // Saved under another extension than the new README
	gh.readmes[1] = fakeReadme{name: "README.md", content: "# New\n"}
	gh.readmes[2] = fakeReadme{name: "README.rst", content: "Two\n"}

	// Day 2: item 1 has a snapshot and isn't in group 2, so only item 2 (no snapshot) is checked
	checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(2), false)
	if err != nil || checked != 1 || gh.readFile(core.ReadmesDBDir+"/01/1.txt") != "Old\n" || gh.readFile(core.ReadmesDBDir+"/02/2.rst") != "Two\n" {
		t.Errorf("day 2: %d %v, files %q", checked, err, gh.files)
	}

	// Day 1: item 1 is in group 1, so its changed README is written.
	// Item 2 now has a snapshot and isn't in group 1
	commitsBefore := len(gh.commits)
	checked, err = core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(1), false)
	_, oldFileLeft := gh.files[core.ReadmesDBDir+"/01/1.txt"]
	if err != nil || checked != 1 || gh.readFile(core.ReadmesDBDir+"/01/1.md") != "# New\n" || oldFileLeft || len(gh.commits) != commitsBefore+1 ||
		gh.commits[len(gh.commits)-1] != "sync(readmes): update readmes of 1 repo\n\nSaved:\n- a/one" {
		t.Errorf("day 1: %d %v, commits %q", checked, err, gh.commits[commitsBefore:])
	}

	// A skipped item isn't asked, even with no snapshot
	delete(gh.files, core.ReadmesDBDir+"/02/2.rst")
	queriesBefore := gh.readmeQueries
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), []int64{2}, syncDay(2), false); err != nil || checked != 0 || gh.readmeQueries != queriesBefore {
		t.Errorf("skipped item: %d %v, README queries %d -> %d", checked, err, queriesBefore, gh.readmeQueries)
	}

	// An item without `node_id` can't be asked by GraphQL, so it isn't asked either
	gh.files[core.StarItemsDBPath] = strings.Replace(gh.files[core.StarItemsDBPath], `"node_id":"R_2",`, "", 1)
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(2), false); err != nil || checked != 0 || gh.readmeQueries != queriesBefore {
		t.Errorf("item without node_id: %d %v, README queries %d -> %d", checked, err, queriesBefore, gh.readmeQueries)
	}
}

// Tests that items with no snapshot are asked before today's group.
// 31 items have no snapshot, and item 61 has one and is in today's group (`61 % 61 == 0`).
// So the first call of 30 asks only items with no snapshot, and item 61 goes in the next call
func TestSyncReadmeSnapshotsWithGitHubAsksUncheckedItemsFirst(t *testing.T) {
	ctx := context.Background()
	items := []string{}
	for _, id := range []int{61, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31} {
		items = append(items, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	gh := setup(t, "[\n"+strings.Join(items, ",\n")+"\n]\n", sampleStarLists)
	repo := openTestDBRepo(t)
	gh.files[core.ReadmesDBDir+"/61/61.md"] = "# Old\n"
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err != nil || checked != 32 {
		t.Fatalf("sync: %d %v", checked, err)
	}
	for _, listedIDs := range gh.readmeListedIDs {
		if slices.Contains(listedIDs, "R_61") && len(listedIDs) != 2 {
			t.Errorf("item 61 was asked with %d items: %v", len(listedIDs), listedIDs)
		}
	}
}

// Tests that items with only the absent marker are asked in every sync, after today's group.
// 31 items (1 to 31) have the absent marker and aren't in today's group, and item 5 has a README on GitHub now.
// Item 61 has a saved README and is in today's group (`61 % 61 == 0`), and item 62 has a saved README and isn't.
// So item 61 goes in the first call of 30, the absent markers fill the rest, and item 62 isn't asked
func TestSyncReadmeSnapshotsWithGitHubAsksAbsentMarkersAfterTodaysGroup(t *testing.T) {
	ctx := context.Background()
	items := []string{}
	for id := 1; id <= 31; id++ {
		items = append(items, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	for _, id := range []int{61, 62} {
		items = append(items, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	gh := setup(t, "[\n"+strings.Join(items, ",\n")+"\n]\n", sampleStarLists)
	repo := openTestDBRepo(t)
	for id := 1; id <= 31; id++ {
		gh.files[fmt.Sprintf("%s/%02d/%d.no-readme", core.ReadmesDBDir, id, id)] = ""
	}
	gh.files[core.ReadmesDBDir+"/61/61.md"] = "# Old\n"
	gh.files[core.ReadmesDBDir+"/62/62.md"] = "# Old\n"
	gh.readmes[5] = fakeReadme{name: "README.md", content: "# Added later\n"}
	gh.readmes[61] = fakeReadme{name: "README.md", content: "# Old\n"}
	gh.readmes[62] = fakeReadme{name: "README.md", content: "# Changed\n"}

	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err != nil || checked != 32 {
		t.Fatalf("sync: %d %v", checked, err)
	}
	_, markerLeft := gh.files[core.ReadmesDBDir+"/05/5.no-readme"]
	if gh.readFile(core.ReadmesDBDir+"/05/5.md") != "# Added later\n" || markerLeft || gh.readFile(core.ReadmesDBDir+"/62/62.md") != "# Old\n" {
		t.Errorf("files %q", gh.files)
	}
	for _, listedIDs := range gh.readmeListedIDs {
		if slices.Contains(listedIDs, "R_62") {
			t.Errorf("item 62 isn't in today's group but was asked: %v", listedIDs)
		}
		if slices.Contains(listedIDs, "R_61") && len(listedIDs) != 30 {
			t.Errorf("item 61 was asked with %d items, not in the first call: %v", len(listedIDs), listedIDs)
		}
	}
}

// Tests that the sync doesn't ask the README of a repository that the metadata step found missing.
// It would only get an absent marker
func TestSyncDoesNotAskReadmesOfMissingRepos(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.gone = map[string]bool{"R_2": true}
	result, err := core.SyncArchiveWithGitHub(ctx, openTestDBRepo(t), testLongStepsEndBy(), false)
	_, hasMarker := gh.files[core.ReadmesDBDir+"/02/2.no-readme"]
	if err != nil || result.NumUnavailableRepos != 1 || hasMarker || slices.ContainsFunc(gh.readmeListedIDs, func(ids []string) bool { return slices.Contains(ids, "R_2") }) {
		t.Errorf("%+v %v, absent marker %v, listed %v", result, err, hasMarker, gh.readmeListedIDs)
	}
}

// Tests that each README fetched in one call is saved for its own item.
// Three items with different READMEs (one of them absent) are asked together
func TestSyncReadmeSnapshotsWithGitHubSavesEachReadmeForItsItem(t *testing.T) {
	ctx := context.Background()
	threeItems := "[\n" +
		`{"id":1,"node_id":"R_1","name":"a/one","note":"","star_list_ids":[],"starred_at":"x"}` + ",\n" +
		`{"id":2,"node_id":"R_2","name":"b/two","note":"","star_list_ids":[],"starred_at":"x"}` + ",\n" +
		`{"id":3,"node_id":"R_3","name":"c/three","note":"","star_list_ids":[],"starred_at":"x"}` + "\n]\n"
	gh := setup(t, threeItems, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.readmes[1] = fakeReadme{name: "README.md", content: "# One\n"}
	gh.readmes[2] = fakeReadme{name: "README.rst", content: "Two\n"}
	checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false)
	_, absent := gh.files[core.ReadmesDBDir+"/03/3.no-readme"]
	if err != nil || checked != 3 || gh.readFile(core.ReadmesDBDir+"/01/1.md") != "# One\n" || gh.readFile(core.ReadmesDBDir+"/02/2.rst") != "Two\n" || !absent {
		t.Errorf("%d %v, files %q", checked, err, gh.files)
	}
}

// Tests that the README sync step deletes the snapshots of items no longer in the archive.
// With nothing to check, it still makes a commit for them, with a subject that says so
func TestSyncReadmeSnapshotsWithGitHubDeletesSnapshotsOfRemovedItems(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.files[core.ReadmesDBDir+"/01/1.md"] = "# One\n" // Saved READMEs, as absent markers are asked in every sync
	gh.files[core.ReadmesDBDir+"/02/2.md"] = "# Two\n"
	gh.files[core.ReadmesDBDir+"/03/3.md"] = "# Removed item\n" // Item 3 isn't in the archive

	checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(3), false) // Group 3: neither item 1 nor item 2
	_, orphanLeft := gh.files[core.ReadmesDBDir+"/03/3.md"]
	if err != nil || checked != 0 || orphanLeft || gh.commits[len(gh.commits)-1] != "sync(readmes): remove readmes of 1 repo no longer in the archive" {
		t.Errorf("%d %v, orphan left %v, commits %q", checked, err, orphanLeft, gh.commits)
	}
}

// Tests that the README sync step deletes no snapshot when the item file reads as missing.
// A missing file reads as an empty archive, and every snapshot would look like one of a removed item
func TestSyncReadmeSnapshotsWithGitHubKeepsSnapshotsWithoutTheItemFile(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.files[core.ReadmesDBDir+"/01/1.md"] = "# One\n"
	gh.files[core.ReadmesDBDir+"/02/2.no-readme"] = ""
	delete(gh.files, core.StarItemsDBPath)
	commitsBefore := len(gh.commits)
	_, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false)
	_, readmeLeft := gh.files[core.ReadmesDBDir+"/01/1.md"]
	_, markerLeft := gh.files[core.ReadmesDBDir+"/02/2.no-readme"]
	if err != nil || !readmeLeft || !markerLeft || len(gh.commits) != commitsBefore {
		t.Errorf("%v, README left %v, marker left %v, commits %d -> %d", err, readmeLeft, markerLeft, commitsBefore, len(gh.commits))
	}
}

// Tests the limits of the README sync step.
//   - More than `maxReadmesPerCommit` (200) items go in several commits
//   - A rate limit answer stops it early without an error, and the READMEs fetched before it are committed
//   - With too little time before `endBy`, it sends no README query.
//     A call starts only if one request time limit is left before the time kept for the commit.
//     With too little time even for the commit, it commits nothing
func TestSyncReadmeSnapshotsWithGitHubLimits(t *testing.T) {
	ctx := context.Background()
	manyItems := []string{}
	for id := 1; id <= 305; id++ {
		manyItems = append(manyItems, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	gh := setup(t, "[\n"+strings.Join(manyItems, ",\n")+"\n]\n", sampleStarLists)
	repo := openTestDBRepo(t)
	commitsBefore := len(gh.commits)
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err != nil || checked != 305 || len(gh.commits) != commitsBefore+2 {
		t.Errorf("305 items: %d %v, commits %d -> %d", checked, err, commitsBefore, len(gh.commits))
	}

	gh = setup(t, "[\n"+strings.Join(manyItems, ",\n")+"\n]\n", sampleStarLists)
	repo = openTestDBRepo(t)
	gh.readmeRateLimitFrom = 7 // The first 6 README queries are answered, then GitHub answers with the rate limit error
	checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false)
	savedFiles := 0
	for path := range gh.files {
		if strings.HasPrefix(path, core.ReadmesDBDir+"/") {
			savedFiles++
		}
	}
	// These items have no README and no `docs` folder, so a call of 30 items is one query (the listing).
	// So the 6 queries answered finish 6 calls.
	// Each goroutine stops after its first rate limit answer
	if err != nil || checked != 180 || savedFiles != checked || gh.readmeQueries > 6+3 {
		t.Errorf("rate limit: %d %v, files %d, README queries %d", checked, err, savedFiles, gh.readmeQueries)
	}

	gh = setup(t, sampleStarItems, sampleStarLists)
	repo = openTestDBRepo(t)
	// 30 seconds is less than the time kept for the commit (40 seconds).
	// 50 seconds leaves 10 seconds before it, less than one request time limit (20 seconds)
	for _, seconds := range []time.Duration{30, 50} {
		if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, time.Now().Add(seconds*time.Second), nil, syncDay(0), false); err != nil || checked != 0 || gh.readmeQueries != 0 {
			t.Errorf("no time (%d seconds): %d %v, README queries %d", seconds, checked, err, gh.readmeQueries)
		}
	}

	// With too little time even for the commit, nothing is committed, not even the deletion of removed items' snapshots
	gh.files[core.ReadmesDBDir+"/03/3.md"] = "# Removed item\n"
	commitsBefore = len(gh.commits)
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, time.Now().Add(10*time.Second), nil, syncDay(0), false); err != nil || checked != 0 || len(gh.commits) != commitsBefore {
		t.Errorf("no time to commit: %d %v, commits %d -> %d", checked, err, commitsBefore, len(gh.commits))
	}
}

// Tests when the README sync step stops because README calls fail.
//   - After a batch whose calls all failed, it stops.
//     The second batch isn't asked, because GitHub is likely failing as a whole or the setup is broken.
//     It isn't an error if the calls failed with a failure that passes on its own (502),
//     and it is one if they failed with a failure that needs a person (401)
//   - A failure about each repository isn't a failed call.
//     Nothing is saved for those repositories, and the second batch is still asked.
//     It is still asked when some calls of the first batch failed and the others fetched nothing, because GitHub answered those
func TestSyncReadmeSnapshotsWithGitHubStopsWhenEveryCallFails(t *testing.T) {
	ctx := context.Background()
	manyItems := []string{}
	for id := 1; id <= 305; id++ {
		manyItems = append(manyItems, fmt.Sprintf(`{"id":%d,"node_id":"R_%d","name":"owner/repo%d","note":"","star_list_ids":[],"starred_at":"x"}`, id, id, id))
	}
	gh := setup(t, "[\n"+strings.Join(manyItems, ",\n")+"\n]\n", sampleStarLists)
	repo := openTestDBRepo(t)
	gh.readmeQueryFails = func(queryNumber int) bool { return true }
	// The first batch (`maxReadmesPerCommit`, 200 items) is 7 calls of up to 30 items, and each fails at its first query
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err != nil || checked != 0 || gh.readmeQueries != 7 {
		t.Errorf("every call fails: %d %v, README queries %d", checked, err, gh.readmeQueries)
	}
	gh.readmeQueries = 0
	gh.readmeFailStatus = http.StatusUnauthorized
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err == nil || checked != 0 || gh.readmeQueries != 7 {
		t.Errorf("every call is refused: %d %v, README queries %d", checked, err, gh.readmeQueries)
	}

	gh = setup(t, "[\n"+strings.Join(manyItems, ",\n")+"\n]\n", sampleStarLists)
	repo = openTestDBRepo(t)
	for id := int64(1); id <= 305; id++ {
		gh.readmes[id] = fakeReadme{unavailable: true}
	}
	// Both batches are asked: 11 calls of one listing query each, with nothing left to read after them
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err != nil || checked != 0 || gh.readmeQueries != 11 {
		t.Errorf("each repository fails: %d %v, README queries %d", checked, err, gh.readmeQueries)
	}

	gh.readmeQueries = 0
	gh.readmeQueryFails = func(queryNumber int) bool { return queryNumber%2 == 1 }
	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false); err != nil || checked != 0 || gh.readmeQueries != 11 {
		t.Errorf("some calls fail, the others fetch nothing: %d %v, README queries %d", checked, err, gh.readmeQueries)
	}
}

// Tests that `refreshEarly` also refreshes the saved READMEs whose turn to be refreshed hasn't come yet.
//   - Without it, an item with a saved README outside today's group isn't asked
//   - With it, that item is asked too, and its changed README is written
func TestSyncReadmeSnapshotsWithGitHubRefreshesEarly(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	readmePath := core.ReadmesDBDir + "/01/1.md"
	gh.files[readmePath] = "# Old\n"
	gh.readmes[1] = fakeReadme{name: "README.md", content: "# New\n"}

	// Day 2: item 1 isn't in group 2
	if _, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(2), false); err != nil || gh.readFile(readmePath) != "# Old\n" {
		t.Errorf("without refreshEarly: %v, README %q", err, gh.readFile(readmePath))
	}
	if _, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(2), true); err != nil || gh.readFile(readmePath) != "# New\n" {
		t.Errorf("with refreshEarly: %v, README %q", err, gh.readFile(readmePath))
	}
}

// Tests how the README sync step reports a commit that fails.
//   - A failure that passes on its own isn't an error, and nothing is saved:
//     GitHub failing on its side (502), the rate limit (403 that says so), and other commits that keep moving the branch
//   - A failure that needs a person is an error: a request GitHub rejects (422)
func TestSyncReadmeSnapshotsWithGitHubCommitFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		treeStatus   int    // Status code of a failing tree creation. 0 if it doesn't fail
		refStatus    int    // Status code of a failing branch move. 0 if it doesn't fail
		refMessage   string // GitHub message of the failing branch move
		interfereAll bool   // Whether another device commits first on every branch move
		wantErr      bool
	}{
		{"GitHub failing", http.StatusBadGateway, 0, "", false, false},
		{"rate limit", 0, http.StatusForbidden, "API rate limit exceeded for user ID 4242.", false, false},
		{"branch keeps moving", 0, 0, "", true, false},
		{"request rejected", http.StatusUnprocessableEntity, 0, "", false, true},
	}
	for _, c := range cases {
		gh := setup(t, sampleStarItems, sampleStarLists)
		repo := openTestDBRepo(t)
		gh.readmes[1] = fakeReadme{name: "README.md", content: "# One\n"}
		gh.treeStatus, gh.refStatus, gh.refMessage, gh.interfereAll = c.treeStatus, c.refStatus, c.refMessage, c.interfereAll
		_, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(0), false)
		_, saved := gh.files[core.ReadmesDBDir+"/01/1.md"]
		if (err != nil) != c.wantErr || saved {
			t.Errorf("%s: %v, README saved %v", c.name, err, saved)
		}
	}
}

// Tests that an ID listed twice in the archive (for example, after a hand edit) doesn't make README writes fail.
//   - The sync step plans the item once, so the same file isn't deleted twice in one commit
//   - Removing both entries deletes the item's snapshots once
func TestReadmeSnapshotsOfAnIDListedTwice(t *testing.T) {
	ctx := context.Background()
	twice := "[\n" +
		`{"id":1,"node_id":"R_1","name":"a/one","note":"","star_list_ids":[],"starred_at":"x"}` + ",\n" +
		`{"id":1,"node_id":"R_1","name":"a/one","note":"","star_list_ids":[],"starred_at":"x"}` + "\n]\n"
	gh := setup(t, twice, sampleStarLists)
	repo := openTestDBRepo(t)
	gh.files[core.ReadmesDBDir+"/01/1.no-readme"] = ""
	gh.readmes[1] = fakeReadme{name: "README.md", content: "# One\n"}

	if checked, err := core.SyncReadmeSnapshotsWithGitHub(ctx, repo, testLongStepsEndBy(), nil, syncDay(1), false); err != nil || checked != 1 || gh.readFile(core.ReadmesDBDir+"/01/1.md") != "# One\n" {
		t.Errorf("sync: %d %v, files %q", checked, err, gh.files)
	}

	_, err := core.UpdateArchive(ctx, repo, func(a *core.Archive) (core.CommitMessage, error) {
		a.StarItems = nil
		return core.NewCommitMessage("user(star-items)", "remove a/one"), nil
	})
	if _, left := gh.files[core.ReadmesDBDir+"/01/1.md"]; err != nil || left {
		t.Errorf("remove: %v, README left %v", err, left)
	}
}

// Tests that a saved README that isn't UTF-8 text still reads back as valid JSON.
// Only a hand edit can leave such a file.
// Its bytes that aren't UTF-8 come as U+FFFD in `text`
func TestReadmeSnapshotThatIsNotUTF8IsReadAsText(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.files[core.ReadmesDBDir+"/01/1.md"] = "\xC7\xD1\xB1\xDB README\n" // "Hangul README" in EUC-KR (two Korean characters)
	rec := call(api.HandleArchiveRepoMeta, "GET", "/api/archive-repo-meta?id=1", "", login(t))
	var answer struct {
		Readme struct {
			State string `json:"state"`
			Text  string `json:"text"`
		} `json:"readme"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &answer)
	if rec.Code != 200 || err != nil || answer.Readme.State != "saved" || !utf8.ValidString(answer.Readme.Text) || !strings.HasSuffix(answer.Readme.Text, " README\n") {
		t.Errorf("%d %v %s", rec.Code, err, rec.Body)
	}
}

// Tests that `core.UpdateDBRepoFiles` writes any bytes as they are.
//   - A file with a NUL byte goes in the tree entry as text, with no blob
//   - A file that isn't UTF-8 goes through a blob, byte for byte
func TestUpdateDBRepoFilesWritesAnyBytes(t *testing.T) {
	ctx := context.Background()
	gh := setup(t, sampleStarItems, sampleStarLists)
	repo := openTestDBRepo(t)
	want := map[string]string{ // Path -> content
		core.ReadmesDBDir + "/02/2.md": "#\x00 \x00h\x00i\x00\n\x00",
		core.ReadmesDBDir + "/01/1.md": "\xC7\xD1\xB1\xDB\n", // EUC-KR
	}
	err := core.UpdateDBRepoFiles(ctx, repo, nil, func(filesRead map[string][]byte, commitSHA string) (core.FileChangesCommit, error) {
		changes := core.FileChangesCommit{FilesToWrite: map[string][]byte{}, Message: core.NewCommitMessage("sync(readmes)", "write test files")}
		for path, content := range want {
			changes.FilesToWrite[path] = []byte(content)
		}
		return changes, nil
	})
	if err != nil || len(gh.blobs) != 1 {
		t.Fatalf("%v, blobs %d", err, len(gh.blobs))
	}
	for path, content := range want {
		if got := gh.readFile(path); got != content {
			t.Errorf("%s: %q, want %q", path, got, content)
		}
	}
}
