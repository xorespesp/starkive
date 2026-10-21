// `/api/archive-repo-meta`: API that reads the snapshots of repositories: metadata (`db/repo-meta.json`) and READMEs (`db/readmes/`).
//
//	GET            -> The raw metadata snapshot file (a JSON array, one repository per line). `[]` if the file doesn't exist yet
//	GET  ?id=<id>  -> { id, readme: { state, format?, text?, truncated? } } (the README snapshot of one item, `core.ReadmeSnapshotContent`)
//	               `state` is `saved`, `absent` (checked, no README or a blank one) or `unchecked` (not checked yet).
//	               `format` is the extension of the saved file, `text` the saved README, `truncated` whether it was cut.
//
// This API only reads.
// Only sync (`/api/sync-archive`) writes the metadata file.
// The dashboard first draws the page from the metadata snapshot.
// When live data comes from GitHub (`/api/github-repo-meta`), it redraws with that.

package handler

import (
	"net/http"

	"github.com/xorespesp/starkive/core"
)

// Returns to a signed-in user the raw metadata snapshot file, or with `?id=` the README snapshot of that item.
// Answers 400 if `id` isn't a positive number, and 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
func handleArchiveRepoMetaGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	if !r.URL.Query().Has("id") {
		return core.WriteDBFile(w, r, cfg, core.RepoMetaDBPath)
	}
	id, err := core.ParseIDParam(r)
	if err != nil {
		return err
	}
	if id <= 0 {
		return core.Errorf(http.StatusBadRequest, "Invalid id")
	}
	readme, err := core.ReadReadmeSnapshot(cfg, id)
	if err != nil {
		return err
	}
	answer := struct {
		ID     int64                      `json:"id"`     // Item ID that was asked
		Readme core.ReadmeSnapshotContent `json:"readme"` // Its README snapshot
	}{ID: id, Readme: readme}
	core.WriteJSON(w, http.StatusOK, answer)
	return nil
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
