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
