// `/api/archive-repo-meta`: API that reads the repository metadata snapshot (`db/repo-meta.json`).
//
//	GET   -> The raw snapshot file (a JSON array, one repository per line). `[]` if the file doesn't exist yet
//
// Only sync (`/api/sync-archive`) writes this file. This API only reads it.
// The dashboard first draws the page from this snapshot.
// When live data comes from GitHub (`/api/github-repo-meta`), it redraws with that.

package handler

import (
	"net/http"

	"github.com/xorespesp/starkive/core"
)

// Returns the raw snapshot file to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
func handleArchiveRepoMetaGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	return core.WriteDBFile(w, r, cfg, core.RepoMetaDBPath)
}

// Entry point for `/api/archive-repo-meta` requests.
// Vercel finds the exported function in this file and deploys it as one function.
// Accepts only GET. Other methods get 405.
func HandleArchiveRepoMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		core.WriteMethodNotAllowed(w)
		return
	}
	core.Respond(w, r, handleArchiveRepoMetaGet)
}
