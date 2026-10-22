// `/api/github-repo-meta`: API that asks GitHub for the current data of repositories (name, description, language, Star count, etc.).
//
//	POST { ids: [node_id, ...] } -> { repos: [...], missing: [node_id, ...] }   (1 to 100 at a time)
//
// `repos` passes on the repository objects from GitHub GraphQL as they are.
// The browser shapes the fields for the page, in one place (`view()` in `public/app.js`).
// `missing` holds the `node_id` of repositories that GitHub clearly answered "not found" for (deleted, made private, etc.).
// A repository that couldn't be read and isn't in `missing` is a temporary failure.
// So the browser doesn't show it as "gone".
// This API only reads, but it uses POST because the ID list goes in the body.

package handler

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/xorespesp/starkive/core"
)

// Characters accepted as a GitHub `node_id`: letters, digits, `_`, `=` and `-`, up to 100 characters.
// Keeps other characters out of the GraphQL variables
var nodeIDPattern = regexp.MustCompile(`^[\w=-]{1,100}$`)

// Asks GitHub for the current data of the given `node_id`s and answers `{ repos, missing }`.
// Answers 400 if there are no IDs, more than 100 IDs, or an ID with a wrong shape.
// If the GitHub request itself fails, it answers with an error (a GitHub failure is 502).
// In that case it doesn't report any repository as gone.
func handleGitHubRepoMetaPost(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}

	var body struct {
		IDs []string `json:"ids"` // `node_id`s of the repositories to ask about
	}
	if err := core.ReadAPIRequestJSON(r, &body); err != nil {
		return err
	}
	if len(body.IDs) == 0 || len(body.IDs) > core.MaxRepoMetaNodeIDs {
		return core.Errorf(http.StatusBadRequest, "Send 1 to %d ids", core.MaxRepoMetaNodeIDs)
	}
	for _, id := range body.IDs {
		if !nodeIDPattern.MatchString(id) {
			return core.Errorf(http.StatusBadRequest, "Invalid id: %s", id)
		}
	}

	nodes, missing, err := core.FetchRepoMetaByNodeIDs(ctx, dbRepo.EnvConfig(), body.IDs)
	if err != nil {
		return err
	}
	repos := []json.RawMessage{} // Starts as an empty array. A `nil` slice becomes `null` in JSON, and the browser would need to handle it
	for _, node := range nodes {
		if node != nil { // A repository that couldn't be read is `nil`, so it is left out
			repos = append(repos, node)
		}
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, map[string]any{"repos": repos, "missing": missing})
	return nil
}

// Entry point for `/api/github-repo-meta` requests.
// Vercel finds the exported function in this file and deploys it as one function.
// Accepts only POST. Other methods get 405.
func HandleGitHubRepoMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		core.WriteAPIMethodNotAllowed(w)
		return
	}
	core.ServeAPIRequest(w, r, handleGitHubRepoMetaPost)
}
