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
// For every write, the server reads the archive at one commit, applies the change, and writes it as one commit (`core.UpdateArchive`).
// The browser sends only what to change.
// So it doesn't overwrite changes made meanwhile on another device, and sending the same Import request again gives the same result.

package handler

import (
	"encoding/json"
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
// It drops `https://`, `www.` and `github.com/` at the front.
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
//   - `change`: A function that gets the whole archive `a` at the same commit and the item `s`.
//     It changes `s` and returns the commit message.
//     If it returns an error, nothing is written.
//     If another commit gets in the way, it may be called again with a new archive
//
// Returns:
//   - The changed item. A 404 `HTTPError` if the item doesn't exist
//
// NOTE: `change` gets the whole archive so that some checks happen at the same commit as the save.
// The star list ID check is one of them: it needs another file (`db/star-lists.json`).
// If that file were read separately, an ID pointing to a star list deleted meanwhile could be saved.
func updateStarItem(cfg *core.Config, id int64, change func(a *core.Archive, s *core.StarItem) (core.CommitMessage, error)) (core.StarItem, error) {
	var updated core.StarItem
	_, err := core.UpdateArchive(cfg, func(a *core.Archive) (core.CommitMessage, error) {
		i := slices.IndexFunc(a.StarItems, func(s core.StarItem) bool { return s.ID == id })
		if i < 0 {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "Not in the archive")
		}
		message, err := change(a, &a.StarItems[i])
		updated = a.StarItems[i] // Set again if called again
		return message, err
	})
	return updated, err
}

// Returns the raw item file to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
func handleArchiveStarItemsGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	return core.WriteDBFile(w, r, cfg, core.StarItemsDBPath)
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

// Looks up one repository on GitHub, adds it to the front of the archive, and answers with the new item as JSON (Add).
// It can add a repository without a Star.
// The time it was added goes where the Star time would be.
// Its README snapshot is saved in the same commit.
// If fetching the README fails (an error, or no answer within `addReadmeTimeout`), the item is added without it and the failure is only logged.
//
// Args:
//   - `fullNameOrURL`: The `owner/repo` or GitHub repository address the user typed
//   - `starListIDs`: IDs of the star lists to put the new item in. If empty, it goes in no star list
//
// Returns:
//   - A 400 `HTTPError` if the input shape is wrong or a star list ID doesn't exist.
//     A 404 if GitHub can't find the repository, and a 409 if it's already in the archive
func addRepoByFullNameOrURL(w http.ResponseWriter, cfg *core.Config, fullNameOrURL string, starListIDs []int64) error {
	fullName := parseRepoFullNameOrURL(fullNameOrURL)
	if fullName == "" {
		return core.Errorf(http.StatusBadRequest, "Enter owner/repo or a GitHub repository URL")
	}
	res, err := core.CallGitHubAPI(cfg, "/repos/"+fullName, core.GitHubAPIRequest{})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// A missing repository and a private one the token can't see are both 404 (docs)
	if res.StatusCode == http.StatusNotFound {
		return core.Errorf(http.StatusNotFound, "Repository not found: %s", fullName)
	}
	if err := core.CheckGitHubAPI(res); err != nil {
		return err
	}
	var repo struct {
		ID       int64  `json:"id"`        // Repository ID. Becomes the item ID
		NodeID   string `json:"node_id"`   // ID for GraphQL lookups
		FullName string `json:"full_name"` // `owner/repo` as GitHub answers it (the letter case may differ from the input)
	}
	if err := json.NewDecoder(res.Body).Decode(&repo); err != nil {
		return err
	}

	readmes, readmeErr := core.FetchReadmesByNodeIDs(cfg, []string{repo.NodeID}, time.Now().Add(addReadmeTimeout))
	if readmeErr == nil && readmes[0] == nil {
		readmeErr = errors.New("GitHub can't tell about the README now") // Sync asks it again later
	}
	if readmeErr != nil {
		log.Printf("add %s: README not saved: %v", repo.FullName, readmeErr)
	}

	item := core.StarItem{ID: repo.ID, NodeID: repo.NodeID, Name: repo.FullName, StarredAt: core.FormatNow()}
	_, err = core.UpdateArchive(cfg, func(a *core.Archive) (core.CommitMessage, error) {
		ids, err := a.CheckStarListIDs(starListIDs) // Reports a bad request (400) before "already there" (409)
		if err != nil {
			return core.CommitMessage{}, err
		}
		if slices.ContainsFunc(a.StarItems, func(s core.StarItem) bool { return s.ID == item.ID }) {
			return core.CommitMessage{}, core.Errorf(http.StatusConflict, "%s is already in the archive", item.Name)
		}
		item.StarListIDs = ids
		a.StarItems = append([]core.StarItem{item}, a.StarItems...)
		if readmeErr == nil {
			a.AddFetchedReadmeToSave(item.ID, *readmes[0])
		}
		return core.NewCommitMessage("user(star-items)", "add "+item.Name), nil
	})
	if err != nil {
		return err
	}
	core.WriteJSON(w, http.StatusOK, item)
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
func addStarredRepoIDs(w http.ResponseWriter, cfg *core.Config, starredRepoIDs []int64, starListIDs []int64) error {
	if len(starredRepoIDs) > maxStarredRepoIDsPerRequest {
		return core.Errorf(http.StatusBadRequest, "Send 1 to %d starred_repo_ids", maxStarredRepoIDsPerRequest)
	}
	starredRepos, err := core.FetchAllStarredRepos(cfg)
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
	added, err := core.ArchiveStarredRepos(cfg, selected, starListIDs, "user(star-items)")
	if err != nil {
		return err
	}
	core.WriteJSON(w, http.StatusOK, map[string]int{"added": added, "skipped": len(chosen) - added})
	return nil
}

// Takes Add and Import requests and handles each by the shape of the body.
// Exactly one of the two fields must be present. Otherwise it answers 400.
func handleArchiveStarItemsPost(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
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
	if err := core.ReadJSON(r, &body); err != nil {
		return err
	}
	if (body.RepoFullNameOrURL == "") == (len(body.StarredRepoIDs) == 0) {
		return core.Errorf(http.StatusBadRequest, "Send either repo_full_name_or_url or starred_repo_ids")
	}
	starListIDs := []int64{} // Whether this ID exists is checked at the commit that saves (`Archive.CheckStarListIDs`)
	if body.StarListID != 0 {
		starListIDs = []int64{body.StarListID}
	}
	if body.RepoFullNameOrURL != "" {
		return addRepoByFullNameOrURL(w, cfg, body.RepoFullNameOrURL, starListIDs)
	}
	return addStarredRepoIDs(w, cfg, body.StarredRepoIDs, starListIDs)
}

// Changes the note or the star lists of one item, and answers with the changed item as JSON.
// The note is trimmed at both ends and its length is checked.
// Answers 400 if `id` isn't a number, the note is too long, or a star list ID doesn't exist.
// Answers 404 if the item doesn't exist.
func handleArchiveStarItemsPatch(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	id, err := core.ParseIDParam(r)
	if err != nil {
		return err
	}
	var body struct {
		Note        *string  `json:"note"`          // New note. A pointer: if the field is missing (`nil`), it doesn't change. An empty string deletes the note
		StarListIDs *[]int64 `json:"star_list_ids"` // The full new set of star lists. A pointer: if missing, it doesn't change. An empty array removes all
	}
	if err := core.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Note != nil {
		trimmed := strings.TrimSpace(*body.Note)
		if err := core.CheckTextLength("Note", trimmed, false, core.MaxNoteLength); err != nil {
			return err
		}
		body.Note = &trimmed
	}
	updated, err := updateStarItem(cfg, id, func(a *core.Archive, s *core.StarItem) (core.CommitMessage, error) {
		commitSummary := "edit " + s.Name
		if body.Note != nil {
			s.Note = *body.Note
			commitSummary = "edit note of " + s.Name
		}
		if body.StarListIDs != nil {
			ids, err := a.CheckStarListIDs(*body.StarListIDs)
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
	core.WriteJSON(w, http.StatusOK, updated)
	return nil
}

// Deletes one item from the archive and answers `{ ok: true }`.
// Its README snapshots are deleted in the same commit (`core.UpdateArchive`).
// Its metadata snapshot is removed in the next sync.
// Answers 400 if `id` isn't a number, and 404 if the item doesn't exist.
func handleArchiveStarItemsDelete(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	id, err := core.ParseIDParam(r)
	if err != nil {
		return err
	}
	_, err = core.UpdateArchive(cfg, func(a *core.Archive) (core.CommitMessage, error) {
		i := slices.IndexFunc(a.StarItems, func(s core.StarItem) bool { return s.ID == id })
		if i < 0 {
			return core.CommitMessage{}, core.Errorf(http.StatusNotFound, "Not in the archive")
		}
		name := a.StarItems[i].Name // `slices.Delete` shifts the later items into place, so the name is taken out before deleting
		a.StarItems = slices.Delete(a.StarItems, i, i+1)
		return core.NewCommitMessage("user(star-items)", "remove "+name), nil
	})
	if err != nil {
		return err
	}
	core.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// Entry point for `/api/archive-star-items` requests. It sends each method to its handler.
// Vercel finds the exported function in this file and deploys it as one function.
// `core.Respond` turns an error from a handler into a JSON error response.
// Other methods get 405.
func HandleArchiveStarItems(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		core.Respond(w, r, handleArchiveStarItemsGet)
	case http.MethodPost:
		core.Respond(w, r, handleArchiveStarItemsPost)
	case http.MethodPatch:
		core.Respond(w, r, handleArchiveStarItemsPatch)
	case http.MethodDelete:
		core.Respond(w, r, handleArchiveStarItemsDelete)
	default:
		core.WriteMethodNotAllowed(w)
	}
}
