// `/api/github-stars`: API that reads my Stars on GitHub, one page at a time.
//
//	GET ?page=<n>   -> { starred_repos, last_page }: One page of Stars (up to 100) and the last page number.
//	                With page=1 it also returns `github_star_lists`: repository ID -> names of the GitHub star lists it belongs to (for display).
//	                If only that part can't be read, it returns `github_star_lists_failed: true` instead of `github_star_lists`
//
// This API only reads from GitHub.
// Adding the chosen Stars to the archive is done by POST on `/api/archive-star-items`.
// Each page is a separate request, so one request reads only one page of Stars.
// (Page 1 also reads the GitHub star list membership.)
// So even with thousands of Stars, no request hits the function time limit, and the browser can fetch several pages at once.

package handler

import (
	"log"
	"net/http"
	"strconv"

	"github.com/xorespesp/starkive/core"
)

// Largest accepted `page` (100 per page, so 100,000 Stars).
// Keeps a bad value from sending useless requests to GitHub
const maxStarredPageNumber = 1000

// Reads one page of Stars from GitHub and answers with it.
// For the first page, it also returns the GitHub star list membership.
// Answers 400 if `page` isn't a number or is outside 1 to `maxStarredPageNumber`.
//
// NOTE: The GitHub star list membership is only shown in the dialog.
// When adding to the archive, the server reads it again from GitHub and decides there (`core.AddStarredReposToStarCatalog`).
// So if only this part can't be read, it still returns the Stars and just adds a failure flag.
// The details go only to the server log.
func handleGitHubStarsGet(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 || page > maxStarredPageNumber {
		return core.Errorf(http.StatusBadRequest, "Invalid page")
	}
	starredRepos, lastPage, err := core.FetchStarredPage(ctx, dbRepo.EnvConfig(), page)
	if err != nil {
		return err
	}
	result := map[string]any{"starred_repos": starredRepos, "last_page": lastPage}
	if page == 1 {
		githubStarLists, err := core.FetchGitHubStarListsByRepo(ctx, dbRepo.EnvConfig())
		if err != nil {
			log.Printf("github star lists: %v", err)
			result["github_star_lists_failed"] = true
		} else {
			result["github_star_lists"] = githubStarLists
		}
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, result)
	return nil
}

// Entry point for `/api/github-stars` requests.
// Vercel finds the exported function in this file and deploys it as one function.
// Accepts only GET. Other methods get 405.
func HandleGitHubStars(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		core.WriteAPIMethodNotAllowed(w)
		return
	}
	core.ServeAPIRequest(w, r, handleGitHubStarsGet)
}
