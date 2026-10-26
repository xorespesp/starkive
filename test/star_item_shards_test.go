// Integration tests for the archive item shards (`core/data_file.go`) through the API, against the fake GitHub
package test

import (
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
