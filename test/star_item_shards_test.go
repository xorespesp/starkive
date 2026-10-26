// Integration tests for the archive item shards (`core/data_file.go`) through the API, against the fake GitHub
package test

import (
	"slices"
	"strings"
	"testing"

	api "github.com/xorespesp/starkive/api"
	"github.com/xorespesp/starkive/core"
)

// Tests that the item list is always the items of one commit, even when a read by branch name shows an older commit.
// The folder listing decides: a shard read that doesn't match its listed blob is read again by that blob
func TestStarItemsGetReadsTheShardsOfOneCommit(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"new"}`, cookie); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	gh.mu.Lock()
	gh.branchReadsLag = true // Reads by branch name now show the commit before the note
	gh.mu.Unlock()
	rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"note":"new"`) || gh.blobReads == 0 {
		t.Errorf("got %d %s, blob reads %d", rec.Code, rec.Body, gh.blobReads)
	}
}

// Tests that a shard whose read by branch name gives a passing 404 is read again by the blob sha of the listing,
// so its items still show
func TestStarItemsGetRereadsAShardThatGaveAPassing404(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	gh.mu.Lock()
	gh.branchReadNotFound = core.BuildStarItemShardPath(shardIndexOf(1)) // The shard of a/one
	gh.mu.Unlock()
	rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"a/one"`) || gh.blobReads != 1 {
		t.Errorf("got %d %s, blob reads %d", rec.Code, rec.Body, gh.blobReads)
	}
}

// Tests that a write of one item reads only the shard of that item and the star lists (`core.UpdateStarItem`).
//   - It refuses to put an item with another ID
//   - An ID that isn't positive, which no item can have, is a 404 for a change and a delete, and nothing is read or written
func TestOneItemWriteReadsOnlyItsShard(t *testing.T) {
	ctx := t.Context()
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	gh.mu.Lock()
	gh.fileReads = nil
	gh.mu.Unlock()
	if rec := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id=2", `{"note":"one shard"}`, cookie); rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	gh.mu.Lock()
	reads := slices.Clone(gh.fileReads)
	gh.mu.Unlock()
	slices.Sort(reads)
	if want := []string{core.BuildStarItemShardPath(2), core.StarListsDBPath}; !slices.Equal(reads, want) {
		t.Errorf("read %q, want %q", reads, want)
	}

	repo := openTestDBRepo(t)
	err := core.UpdateStarItem(ctx, repo, 2, func(tx *core.StarItemTransaction) (core.CommitMessage, error) {
		item, _ := tx.Item()
		item.ID = 10 // Another item in the same shard (10 % 8 is 2), so only the ID check of `SetItem` can refuse it
		return core.NewCommitMessage("user(star-items)", "edit"), tx.SetItem(item)
	})
	if err == nil || !strings.Contains(gh.readStarItems(), `"id":2,`) {
		t.Errorf("another ID: %v", err)
	}

	commitsBefore := len(gh.commits)
	for _, id := range []string{"0", "-8"} {
		gh.mu.Lock()
		gh.fileReads = nil
		gh.mu.Unlock()
		patch := call(api.HandleArchiveStarItems, "PATCH", "/api/archive-star-items?id="+id, `{"note":"x"}`, cookie)
		remove := call(api.HandleArchiveStarItems, "DELETE", "/api/archive-star-items?id="+id, "", cookie)
		if patch.Code != 404 || remove.Code != 404 || len(gh.fileReads) != 0 || len(gh.commits) != commitsBefore {
			t.Errorf("ID %s: patch %d, delete %d, reads %q, commits %d -> %d", id, patch.Code, remove.Code, gh.fileReads, commitsBefore, len(gh.commits))
		}
	}
}

// Returns how many reads of archive item shards the fake GitHub answered with the content, not with 304
func countShardDownloads(gh *fakeGitHub) int {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	count := 0
	for _, path := range gh.fileDownloads {
		if strings.HasPrefix(path, core.StarItemsDBDir+"/") {
			count++
		}
	}
	return count
}

// Tests that one sync downloads each archive item shard only once, though it reads the archive several times.
// The later reads ask with the `ETag` of the first, and GitHub answers 304 for the shards that didn't change.
// A shard the sync wrote itself (a new Star) answers 304 too, because the write keeps it with its `ETag`
func TestSyncDownloadsEachShardOnce(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = []string{"a/three"} // One new Star, so the sync writes shard 3 before the later reads
	gh.mu.Lock()
	gh.fileDownloads = nil
	gh.mu.Unlock()
	if rec := callScheduledSync(); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"synced"}` {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	if downloads := countShardDownloads(gh); downloads != core.StarItemShardCount || !strings.Contains(gh.readStarItems(), "a/three") {
		t.Errorf("shard downloads %d, archive %s", downloads, gh.readStarItems())
	}
}

// Tests that a sync whose first commit loses to another device's commit keeps both changes,
// and that its retry downloads again only the shard the other commit changed (the others answer 304)
func TestSyncRetryDownloadsOnlyTheShardAnotherWriteChanged(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	gh.starred = []string{"a/three"} // One new Star, so the sync's first write is a whole-catalog write
	gh.mu.Lock()
	gh.fileDownloads = nil
	// Another device edits the note of b/two (shard 2) right before the sync's first commit moves the branch
	gh.interfereOnce = func(files map[string]string) {
		putStarItems(files, strings.Replace(joinStarItems(files), `"name":"b/two","note":""`, `"name":"b/two","note":"other device"`, 1))
	}
	gh.mu.Unlock()
	if rec := callScheduledSync(); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"status":"synced"}` {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	archive := gh.readStarItems()
	if downloads := countShardDownloads(gh); downloads != core.StarItemShardCount+1 ||
		!strings.Contains(archive, "a/three") || !strings.Contains(archive, `"note":"other device"`) {
		t.Errorf("shard downloads %d, archive %s", downloads, archive)
	}
}

// Tests that deleting a star list whose members are in several shards cleans up every member in one commit.
// The members are a/one and i/nine (shard 1) and b/two (shard 2), and b/two keeps its other star list
func TestStarListDeleteCleansUpMembersInEveryShard(t *testing.T) {
	gh := setup(t, "[\n"+
		`{"id":1,"node_id":"R_1","name":"a/one","note":"","star_list_ids":[100],"starred_at":"2026-01-01T00:00:00Z"}`+",\n"+
		`{"id":2,"node_id":"R_2","name":"b/two","note":"","star_list_ids":[100,200],"starred_at":"2026-01-01T00:00:00Z"}`+",\n"+
		`{"id":9,"node_id":"R_9","name":"i/nine","note":"","star_list_ids":[100],"starred_at":"2026-01-01T00:00:00Z"}`+
		"\n]\n", sampleStarLists)
	cookie := login(t)
	commitsBefore := len(gh.commits)
	if rec := call(api.HandleArchiveStarLists, "DELETE", "/api/archive-star-lists?id=100", "", cookie); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	shard1, shard2 := gh.readFile(core.BuildStarItemShardPath(1)), gh.readFile(core.BuildStarItemShardPath(2))
	if strings.Contains(shard1, "100") || strings.Count(shard1, `"star_list_ids":[]`) != 2 || !strings.Contains(shard2, `"star_list_ids":[200]`) {
		t.Errorf("shard 1 %s, shard 2 %s", shard1, shard2)
	}
	if len(gh.commits) != commitsBefore+1 || !strings.Contains(gh.commits[len(gh.commits)-1], "Removed from 3 repos") {
		t.Errorf("commits: %q", gh.commits[commitsBefore:])
	}
}

// Tests that a missing shard is a 502 for the list and for a write, and nothing is read as an empty archive or written
func TestMissingStarItemShardIsBadGateway(t *testing.T) {
	gh := setup(t, sampleStarItems, sampleStarLists)
	cookie := login(t)
	delete(gh.files, core.BuildStarItemShardPath(5))
	commitsBefore := len(gh.commits)
	if rec := call(api.HandleArchiveStarItems, "GET", "/api/archive-star-items", "", cookie); rec.Code != 502 || !strings.Contains(rec.Body.String(), "05.json") {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(api.HandleArchiveStarLists, "DELETE", "/api/archive-star-lists?id=100", "", cookie); rec.Code != 502 || len(gh.commits) != commitsBefore {
		t.Errorf("delete star list: %d %s, commits %d -> %d", rec.Code, rec.Body, commitsBefore, len(gh.commits))
	}
}
