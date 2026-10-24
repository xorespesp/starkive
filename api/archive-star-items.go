// `/api/archive-star-items`: API that reads and changes the archive items (`db/star-items.json`).
// Add and delete also write or delete the item's README snapshot (`db/readmes/`) in the same commit.
//
//	GET              -> The raw item file (a JSON array, one item per line). `[]` if the file doesn't exist yet
//	POST             { repo_full_name_or_url, star_list_id? } -> The added item (Add: adds one repository directly, without a Star)
//	                 `repo_full_name_or_url` is `owner/repo` or a GitHub repository address.
//	                 `star_list_id` is the star list to put it in (none if missing or 0)
//	POST             { starred_repo_ids: [...], star_list_id? } -> { added, skipped } (Import: adds the chosen ones from my GitHub Stars at once)
//	PATCH  ?id=<id>  { note?, star_list_ids? } -> The changed item. Only the fields that were sent change
//	DELETE ?id=<id>  -> { ok: true }
//
// For every write, the server reads the star catalog at one commit, applies the change, and writes it as one commit (`core.UpdateStarCatalog`).
// The browser sends only what to change.
// So it doesn't overwrite changes made meanwhile on another device, and sending the same Import request again gives the same result.

package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Characters allowed in one part of a repository name (the owner or the repository): letters, digits, `_`, `.` and `-`
var repoNamePart = regexp.MustCompile(`^[\w.-]+$`)

// Extracts `owner/repo` from what the user typed (`owner/repo` or a GitHub repository address).
// It drops `https://` or `http://`, `www.` and `github.com/` at the front.
// It also drops `?...`, `#...`, paths like `/tree/main`, and `.git` at the end.
//
// Args:
//   - `fullNameOrURL`: The text the user typed
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

// Finds one archive item, changes it with `change`, and commits.
//
// Args:
//   - `id`: ID of the item to change (the GitHub repository ID)
//   - `change`: A function that gets the transaction `tx` (the catalog read at one commit) and the item `s` in it.
//     It changes `s` and returns the commit message.
//     If it returns an error, nothing is written.
//     If another commit gets in the way, it may be called again with a new transaction
//
// Returns:
//   - The changed item. A 404 `HTTPError` if the item doesn't exist
//
// NOTE: `change` gets the whole catalog so that some checks happen at the same commit as the save.
// The star list ID check is one of them: it needs another file (`db/star-lists.json`).
// If that file were read separately, an ID pointing to a star list deleted meanwhile could be saved.
func updateStarItem(ctx context.Context, dbRepo *core.DBRepo, id int64, change func(tx *core.StarCatalogTransaction, s *core.StarItem) (core.CommitMessage, error)) (core.StarItem, error) {
	var updated core.StarItem
	_, err := core.UpdateStarCatalog(ctx, dbRepo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) {
		i := slices.IndexFunc(tx.StarItems, func(s core.StarItem) bool { return s.ID == id })
		if i < 0 {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "Not in the archive")
		}
		message, err := change(tx, &tx.StarItems[i])
		updated = tx.StarItems[i] // Set again if called again
		return message, err
	})
	return updated, err
}

// Returns the raw item file to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
func handleArchiveStarItemsGet(w http.ResponseWriter, r *http.Request) error {
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	return core.WriteAPIDBFileResponse(w, r, dbRepo, core.StarItemsDBPath)
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

// Result of looking up the repository to add on GitHub
type repoToAdd struct {
	item   core.StarItem       // The new item, without star lists
	readme *core.FetchedReadme // Its README. `nil` if it wasn't fetched
	err    error               // Why the repository couldn't be looked up. A README that wasn't fetched isn't an error
}

// Looks up the repository `fullName` on GitHub and fetches its README.
// If fetching the README fails (an error, or no answer within `addReadmeTimeout`), the result has no README and the failure is only logged.
//
// Returns:
//   - The new item and its README.
//     `err` is set if the lookup fails: a 404 `HTTPError` if GitHub can't find the repository, or the GitHub error
func lookUpRepoToAdd(ctx context.Context, dbRepo *core.DBRepo, fullName string) repoToAdd {
	res, err := core.CallGitHubAPI(ctx, dbRepo.EnvConfig(), "/repos/"+fullName, core.GitHubAPIRequest{})
	if err != nil {
		return repoToAdd{err: err}
	}
	defer res.Body.Close()
	// A missing repository and a private one the token can't see are both 404 (docs)
	if res.StatusCode == http.StatusNotFound {
		return repoToAdd{err: core.Errorf(http.StatusNotFound, "Repository not found: %s", fullName)}
	}
	if err := core.CheckGitHubResponse(res); err != nil {
		return repoToAdd{err: err}
	}
	var repo struct {
		ID       int64  `json:"id"`        // Repository ID. Becomes the item ID
		NodeID   string `json:"node_id"`   // ID for GraphQL lookups
		FullName string `json:"full_name"` // `owner/repo` as GitHub answers it (the letter case may differ from the input)
	}
	if err := core.DecodeGitHubResponse(res, &repo); err != nil {
		return repoToAdd{err: err}
	}
	found := repoToAdd{item: core.StarItem{ID: repo.ID, NodeID: repo.NodeID, Name: repo.FullName, StarredAt: core.FormatNow()}}

	// Only fetching the README gets the shorter deadline `addReadmeTimeout`, so a slow GitHub doesn't keep the user waiting.
	// `WithTimeout` makes it a child of `ctx`, so it never ends later than the deadline of the request.
	// The commit uses `ctx`, not this child, so a slow README can't make the save fail.
	// `cancel` frees the timer of the child right away, because nothing after the README uses it
	addReadmeCtx, cancel := context.WithTimeout(ctx, addReadmeTimeout)
	readmes, readmeErr := core.FetchReadmesByNodeIDs(addReadmeCtx, dbRepo.EnvConfig(), []string{repo.NodeID})
	cancel()
	if readmeErr == nil && readmes[0] == nil {
		readmeErr = errors.New("GitHub can't tell about the README now") // Sync asks it again later
	}
	if readmeErr != nil {
		log.Printf("add %s: README not saved: %v", repo.FullName, readmeErr)
		return found
	}
	found.readme = readmes[0]
	return found
}

// Looks up one repository on GitHub, adds it to the front of the archive, and answers with the new item as JSON (Add).
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
// NOTE: The lookup on GitHub (`lookUpRepoToAdd`) and the start of the save run at the same time.
// They don't need each other until `change` adds the item.
// The lookup takes about 0.8 seconds, and reading the files before `change` about 0.7 seconds with 20,000 items (measured).
// So Add ends sooner by about the shorter of the two.
// The lookup is taken once and used again when `change` runs again after a conflict.
func addRepoByFullNameOrURL(ctx context.Context, w http.ResponseWriter, dbRepo *core.DBRepo, fullNameOrURL string, starListIDs []int64) error {
	fullName := parseRepoFullNameOrURL(fullNameOrURL)
	if fullName == "" {
		return core.Errorf(http.StatusBadRequest, "Enter owner/repo or a GitHub repository URL")
	}
	// `chan`: a typed queue between goroutines.
	// Every path below takes the result (`takeLookup`), so the goroutine ends before this function returns.
	// Room for one result keeps the send from waiting for that, so the goroutine can't be left blocked.
	lookupDone := make(chan repoToAdd, 1)
	// `go`: runs the function in a goroutine (a lightweight thread), so the save below starts without waiting.
	// `lookupDone <- v` sends `v` to the channel
	go func() { lookupDone <- lookUpRepoToAdd(ctx, dbRepo, fullName) }()
	var lookup *repoToAdd // `nil` until the result is taken from `lookupDone`
	// Returns the lookup result.
	// The first call waits for it, and later calls get the same result
	takeLookup := func() repoToAdd {
		if lookup == nil {
			found := <-lookupDone // Waits until the goroutine sends its result
			lookup = &found
		}
		return *lookup
	}

	var item core.StarItem
	_, err := core.UpdateStarCatalog(ctx, dbRepo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) {
		found := takeLookup()
		if found.err != nil {
			return core.CommitMessage{}, found.err
		}
		item = found.item
		ids, err := tx.CheckStarListIDs(starListIDs) // Reports a bad request (400) before "already there" (409)
		if err != nil {
			return core.CommitMessage{}, err
		}
		if slices.ContainsFunc(tx.StarItems, func(s core.StarItem) bool { return s.ID == item.ID }) {
			return core.CommitMessage{}, core.Errorf(http.StatusConflict, "%s is already in the archive", item.Name)
		}
		item.StarListIDs = ids
		tx.StarItems = append([]core.StarItem{item}, tx.StarItems...)
		if found.readme != nil {
			tx.SaveReadme(item.ID, *found.readme)
		}
		return core.NewCommitMessage("user(star-items)", "add "+item.Name), nil
	})
	if err != nil {
		// The save can fail before `change` runs (for example, GitHub fails while the files are read).
		// A repository that doesn't exist still answers 404, not with the error of the save.
		// So this waits for the lookup if it is still running
		if found := takeLookup(); found.err != nil {
			return found.err
		}
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
	updated, err := updateStarItem(ctx, dbRepo, id, func(tx *core.StarCatalogTransaction, s *core.StarItem) (core.CommitMessage, error) {
		commitSummary := "edit " + s.Name
		if body.Note != nil {
			s.Note = *body.Note
			commitSummary = "edit note of " + s.Name
		}
		if body.StarListIDs != nil {
			ids, err := tx.CheckStarListIDs(*body.StarListIDs)
			if err != nil {
				return core.CommitMessage{}, err
			}
			s.StarListIDs = ids
			commitSummary = "change star lists of " + s.Name
		}
		return core.NewCommitMessage("user(star-items)", commitSummary), nil
	})
	if err != nil {
		return err
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, updated)
	return nil
}

// Deletes one item from the archive and answers `{ ok: true }`.
// Its README snapshots are deleted in the same commit (`core.UpdateStarCatalog`).
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
	_, err = core.UpdateStarCatalog(ctx, dbRepo, func(tx *core.StarCatalogTransaction) (core.CommitMessage, error) {
		i := slices.IndexFunc(tx.StarItems, func(s core.StarItem) bool { return s.ID == id })
		if i < 0 {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "Not in the archive")
		}
		name := tx.StarItems[i].Name // `slices.Delete` shifts the later items into place, so the name is taken out before deleting
		tx.StarItems = slices.Delete(tx.StarItems, i, i+1)
		return core.NewCommitMessage("user(star-items)", "remove "+name), nil
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
