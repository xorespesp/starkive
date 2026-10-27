// `/api/archive-star-items`: API that reads and changes the archive items (the shards under `db/star-items/`).
// Add and delete also write or delete the item's README snapshot (`db/readmes/`) in the same commit.
//
//	GET              -> All items as one JSON array, one item per line
//	POST             { repo_full_name_or_url, star_list_id? } -> The added item (Add: adds one repository directly, without a Star)
//	                 `repo_full_name_or_url` is `owner/repo` or a GitHub repository address.
//	                 `star_list_id` is the star list to put it in (none if missing or 0)
//	POST             { starred_repo_ids: [...], star_list_id? } -> { added, skipped } (Import: adds the chosen ones from my GitHub Stars at once)
//	PATCH  ?id=<id>  { note?, star_list_ids? } -> The changed item. Only the fields that were sent change
//	DELETE ?id=<id>  -> { ok: true }
//
// For every write, the server reads the data it needs at one commit, applies the change, and writes it as one commit
// (`core.UpdateStarItem` for one item, `core.UpdateStarCatalog` for Import).
// The browser sends only what to change.
// So it doesn't overwrite changes made meanwhile on another device, and sending the same Import request again gives the same result.

package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Characters allowed in one part of a repository name (the owner or the repository): letters, digits, `_`, `.` and `-`
var repoNamePart = regexp.MustCompile(`^[\w.-]+$`)

// Extracts `owner/repo` from `fullNameOrURL`, the text the user typed (`owner/repo` or a GitHub repository address).
// It drops `https://` or `http://`, `www.` and `github.com/` at the front.
// It also drops `?...`, `#...`, paths like `/tree/main`, and `.git` at the end.
//
// Returns:
//   - `owner/repo`. An empty string if the shape is wrong
//
// NOTE: The result goes as is into a GitHub API path (`/repos/<owner/repo>`).
// Without blocking parts like `..`, `../user` would turn into `/repos/../user`, which is `/user`.
// That could call a different GitHub API, so parts made only of dots are rejected.
func parseRepoFullNameOrURL(fullNameOrURL string) string {
	s := strings.TrimSpace(fullNameOrURL)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimPrefix(s, "www.")
	if strings.HasPrefix(strings.ToLower(s), "github.com/") {
		s = s[len("github.com/"):]
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, "/") // Uses only the first two parts and drops a path like `/tree/main` after them
	if len(parts) < 2 {
		return ""
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	for _, part := range []string{owner, name} {
		if !repoNamePart.MatchString(part) || strings.Trim(part, ".") == "" {
			return ""
		}
	}
	return owner + "/" + name
}

// Returns all archive items to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
func handleArchiveStarItemsGet(w http.ResponseWriter, r *http.Request) error {
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	return core.WriteAPIAllStarItemsResponse(w, r, dbRepo)
}

// Largest number of repository IDs in one Import request.
// The request holds only IDs and is small, so the limit is generous.
// It only keeps a bad request from doing a needlessly large job
const maxStarredRepoIDsPerRequest = 100_000

// Longest time Add waits for the README of the new repository.
// Fetching one README (two or three GraphQL queries, more if a README is binary) usually takes about 1 second.
// The slowest of 20 tries took 1.6 seconds (measured).
// One that doesn't come in time, or fails, is fetched by sync, so Add doesn't keep the user waiting
const addReadmeTimeout = 5 * time.Second

// Looks up the repository `fullName` on GitHub, and makes the new item for it (without star lists).
//
// Returns:
//   - The new item.
//     A 404 `HTTPError` if GitHub can't find the repository, or the GitHub error
func lookUpRepoToAdd(ctx context.Context, dbRepo *core.DBRepo, fullName string) (core.StarItem, error) {
	res, err := core.CallGitHubAPI(ctx, dbRepo.EnvConfig(), "/repos/"+fullName, core.GitHubAPIRequest{})
	if err != nil {
		return core.StarItem{}, err
	}
	defer res.Body.Close()
	// A missing repository and a private one the token can't see are both 404 (docs)
	if res.StatusCode == http.StatusNotFound {
		return core.StarItem{}, core.Errorf(http.StatusNotFound, "Repository not found: %s", fullName)
	}
	if err := core.CheckGitHubResponse(res); err != nil {
		return core.StarItem{}, err
	}
	var repo struct {
		ID       int64  `json:"id"`        // Repository ID. Becomes the item ID
		NodeID   string `json:"node_id"`   // ID for GraphQL lookups
		FullName string `json:"full_name"` // `owner/repo` as GitHub answers it (the letter case may differ from the input)
	}
	if err := core.DecodeGitHubResponse(res, &repo); err != nil {
		return core.StarItem{}, err
	}
	return core.StarItem{ID: repo.ID, NodeID: repo.NodeID, Name: repo.FullName, StarredAt: core.FormatNow()}, nil
}

// Fetches the README of the repository to add, whose `node_id` is `nodeID`.
// `name` is the repository name, only for the log.
// If it fails (an error, or no answer within `addReadmeTimeout`), the failure is only logged, and sync fetches it later.
//
// Returns:
//   - The README. `nil` if it wasn't fetched
func fetchReadmeToAdd(ctx context.Context, dbRepo *core.DBRepo, nodeID string, name string) *core.FetchedReadme {
	// Only fetching the README gets the shorter deadline `addReadmeTimeout`.
	// `WithTimeout` makes it a child of `ctx`, so it never ends later than the deadline of the request.
	// The commit uses `ctx`, not this child, so a slow README can't make the save fail.
	// `cancel` frees the timer of the child right away, because nothing after the README uses it
	addReadmeCtx, cancel := context.WithTimeout(ctx, addReadmeTimeout)
	readmes, err := core.FetchReadmesByNodeIDs(addReadmeCtx, dbRepo.EnvConfig(), []string{nodeID})
	cancel()
	if err == nil && readmes[0] == nil {
		err = errors.New("GitHub can't tell about the README now") // Sync asks it again later
	}
	if err != nil {
		if ctx.Err() == nil { // Cut at the end of the request (the save failed first, so nobody waits for it): nothing to report
			log.Printf("add %s: README not saved: %v", name, err)
		}
		return nil
	}
	return readmes[0]
}

// Looks up one repository on GitHub, adds it to the archive, and answers with the new item as JSON (Add).
// It can add a repository without a Star.
// The time it was added goes where the Star time would be.
// Its README snapshot is saved in the same commit.
// If the README can't be fetched, the item is added without it.
//
// Args:
//   - `fullNameOrURL`: The `owner/repo` or GitHub repository address the user typed
//   - `starListIDs`: IDs of the star lists to put the new item in. If empty, it goes in no star list
//
// Returns:
//   - A 400 `HTTPError` if the input shape is wrong or a star list ID doesn't exist.
//     A 404 if GitHub can't find the repository, and a 409 if it's already in the archive
//
// NOTE: The save reads only the shard of the new item, so it needs the repository ID first.
// The repository lookup (`lookUpRepoToAdd`) is quick and comes first.
// The README (`fetchReadmeToAdd`, up to `addReadmeTimeout`) is fetched at the same time as the save reads its files.
// `change` waits for the README only when it adds the item.
// The README is taken once and used again when `change` runs again after a conflict.
func addRepoByFullNameOrURL(ctx context.Context, w http.ResponseWriter, dbRepo *core.DBRepo, fullNameOrURL string, starListIDs []int64) error {
	fullName := parseRepoFullNameOrURL(fullNameOrURL)
	if fullName == "" {
		return core.Errorf(http.StatusBadRequest, "Enter owner/repo or a GitHub repository URL")
	}
	found, err := lookUpRepoToAdd(ctx, dbRepo, fullName)
	if err != nil {
		return err
	}
	// `chan`: a typed queue between goroutines.
	// Room for one result keeps the send from waiting, so the goroutine ends on its own even if the save fails before taking it.
	// `readmeDone <- v` sends `v` to the channel.
	// A panic in the fetch becomes a missing README (`core.RunWithPanicAsError`), so the goroutine still sends one result
	readmeDone := make(chan *core.FetchedReadme, 1)
	go func() { // `go`: runs the function in a goroutine (a lightweight thread), so the save below starts without waiting
		var readme *core.FetchedReadme
		if err := core.RunWithPanicAsError(func() error { readme = fetchReadmeToAdd(ctx, dbRepo, found.NodeID, found.Name); return nil }); err != nil {
			log.Printf("add %s: README not saved: %v", found.Name, err)
		}
		readmeDone <- readme
	}()
	// Returns the README.
	// `sync.OnceValue` runs the function on the first call only, so the first call waits for the goroutine,
	// and later calls get the same result
	takeReadme := sync.OnceValue(func() *core.FetchedReadme { return <-readmeDone })

	item := found
	err = core.UpdateStarItem(ctx, dbRepo, found.ID, func(tx *core.StarItemTransaction) (core.CommitMessage, error) {
		item = found                                 // Set again if called again
		ids, err := tx.CheckStarListIDs(starListIDs) // Reports a bad request (400) before "already there" (409)
		if err != nil {
			return core.CommitMessage{}, err
		}
		if _, exists := tx.Item(); exists { // The same ID can only be in this item's shard, so this is the whole archive's answer
			return core.CommitMessage{}, core.Errorf(http.StatusConflict, "%s is already in the archive", item.Name)
		}
		item.StarListIDs = ids
		if err := tx.SetItem(item); err != nil {
			return core.CommitMessage{}, err
		}
		if fetched := takeReadme(); fetched != nil {
			tx.AddReadmeToCommit(*fetched)
		}
		return core.NewCommitMessage("user(star-items)", "add "+item.Name), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, item)
	return nil
}

// Adds the chosen ones from my GitHub Stars to the archive at once, and answers `{ added, skipped }` (Import).
// It skips IDs already in the archive and IDs that aren't my Stars (`skipped`).
// New items also go into the star lists they belonged to on GitHub.
//
// Args:
//   - `starredRepoIDs`: Repository IDs chosen in the browser
//   - `starListIDs`: IDs of the star lists to put all new items in. If empty, they go only into their GitHub star lists
//
// Returns:
//   - A 400 `HTTPError` if there are too many IDs or a star list ID doesn't exist
//
// NOTE: The browser sends only "what to pick".
// The server reads the data to save (name, Star time, star lists) again, directly from GitHub.
// So the request is small and doesn't need to be split.
// Even if the list in the browser is stale or tampered with, the saved data is correct.
// The cost is that the server reads all Stars once more.
func addStarredRepoIDs(ctx context.Context, w http.ResponseWriter, dbRepo *core.DBRepo, starredRepoIDs []int64, starListIDs []int64) error {
	if len(starredRepoIDs) > maxStarredRepoIDsPerRequest {
		return core.Errorf(http.StatusBadRequest, "Send 1 to %d starred_repo_ids", maxStarredRepoIDsPerRequest)
	}
	starredRepos, err := core.FetchAllStarredRepos(ctx, dbRepo.EnvConfig())
	if err != nil {
		return err
	}
	chosen := make(map[int64]bool, len(starredRepoIDs))
	for _, id := range starredRepoIDs {
		chosen[id] = true
	}
	selected := []core.StarredRepo{} // Keeps GitHub's order (most recently starred first, docs)
	for _, s := range starredRepos {
		if chosen[s.Repo.ID] {
			selected = append(selected, s)
		}
	}
	added, err := core.AddStarredReposToStarCatalog(ctx, dbRepo, selected, starListIDs, "user(star-items)")
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, map[string]int{"added": added, "skipped": len(chosen) - added})
	return nil
}

// Takes Add and Import requests and handles each by the shape of the body.
// Exactly one of `repo_full_name_or_url` and `starred_repo_ids` must be present.
// Otherwise it answers 400.
func handleArchiveStarItemsPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	// The request body can be read only once.
	// So one struct takes the fields of both shapes, and the filled one decides which request it is
	var body struct {
		RepoFullNameOrURL string  `json:"repo_full_name_or_url"` // Add: the one repository to add directly
		StarredRepoIDs    []int64 `json:"starred_repo_ids"`      // Import: the repository IDs chosen from my GitHub Stars
		StarListID        int64   `json:"star_list_id"`          // Star list for the new items. 0 means none
	}
	if err := core.ReadAPIRequestJSON(r, &body); err != nil {
		return err
	}
	if (body.RepoFullNameOrURL == "") == (len(body.StarredRepoIDs) == 0) {
		return core.Errorf(http.StatusBadRequest, "Send either repo_full_name_or_url or starred_repo_ids")
	}
	starListIDs := []int64{} // Whether this ID exists is checked at the commit that saves (`StarCatalog.CheckStarListIDs`)
	if body.StarListID != 0 {
		starListIDs = []int64{body.StarListID}
	}
	if body.RepoFullNameOrURL != "" {
		return addRepoByFullNameOrURL(ctx, w, dbRepo, body.RepoFullNameOrURL, starListIDs)
	}
	return addStarredRepoIDs(ctx, w, dbRepo, body.StarredRepoIDs, starListIDs)
}

// Changes the note or the star lists of one item, and answers with the changed item as JSON.
// The note is trimmed at both ends and its length is checked.
// Answers 400 if `id` isn't a number, the note is too long, or a star list ID doesn't exist.
// Answers 404 if the item doesn't exist.
func handleArchiveStarItemsPatch(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	id, err := core.ParseAPIRequestID(r)
	if err != nil {
		return err
	}
	var body struct {
		Note        *string  `json:"note"`          // New note. A pointer: if the field is missing (`nil`), it doesn't change. An empty string deletes the note
		StarListIDs *[]int64 `json:"star_list_ids"` // The full new set of star lists. A pointer: if missing, it doesn't change. An empty array removes all
	}
	if err := core.ReadAPIRequestJSON(r, &body); err != nil {
		return err
	}
	if body.Note != nil {
		trimmed := strings.TrimSpace(*body.Note)
		if err := core.CheckTextLength("Note", trimmed, false, core.MaxNoteLength); err != nil {
			return err
		}
		body.Note = &trimmed
	}
	var updated core.StarItem
	err = core.UpdateStarItem(ctx, dbRepo, id, func(tx *core.StarItemTransaction) (core.CommitMessage, error) {
		item, found := tx.Item()
		if !found {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "Not in the archive")
		}
		commitSummary := "edit " + item.Name
		if body.Note != nil {
			item.Note = *body.Note
			commitSummary = "edit note of " + item.Name
		}
		if body.StarListIDs != nil {
			ids, err := tx.CheckStarListIDs(*body.StarListIDs)
			if err != nil {
				return core.CommitMessage{}, err
			}
			item.StarListIDs = ids
			commitSummary = "change star lists of " + item.Name
		}
		if err := tx.SetItem(item); err != nil {
			return core.CommitMessage{}, err
		}
		updated = item // Set again if called again
		return core.NewCommitMessage("user(star-items)", commitSummary), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, updated)
	return nil
}

// Deletes one item from the archive and answers `{ ok: true }`.
// Its README snapshots are deleted in the same commit (`core.UpdateStarItem`).
// Its metadata snapshot is removed in the next sync.
// Answers 400 if `id` isn't a number, and 404 if the item doesn't exist.
func handleArchiveStarItemsDelete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	id, err := core.ParseAPIRequestID(r)
	if err != nil {
		return err
	}
	err = core.UpdateStarItem(ctx, dbRepo, id, func(tx *core.StarItemTransaction) (core.CommitMessage, error) {
		item, found := tx.Item()
		if !found {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "Not in the archive")
		}
		tx.RemoveItem()
		return core.NewCommitMessage("user(star-items)", "remove "+item.Name), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// Entry point for `/api/archive-star-items` requests. It sends each method to its handler.
// Vercel finds the exported function in this file and deploys it as one function.
// `core.ServeAPIRequest` turns an error from a handler into a JSON error response.
// Other methods get 405.
func HandleArchiveStarItems(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		core.ServeAPIRequest(w, r, handleArchiveStarItemsGet)
	case http.MethodPost:
		core.ServeAPIRequest(w, r, handleArchiveStarItemsPost)
	case http.MethodPatch:
		core.ServeAPIRequest(w, r, handleArchiveStarItemsPatch)
	case http.MethodDelete:
		core.ServeAPIRequest(w, r, handleArchiveStarItemsDelete)
	default:
		core.WriteAPIMethodNotAllowed(w)
	}
}
