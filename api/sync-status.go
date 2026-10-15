// `/api/sync-status`: API that reads the auto sync status.
//
//	GET   -> `core.SyncStatus`: the auto sync setting in `config.json`, and the workflow state and last scheduled run on GitHub Actions
//
// Turning auto sync on and off, changing the interval and syncing right away are in `/api/sync-archive`.

package handler

import (
	"net/http"

	"github.com/xorespesp/starkive/core"
)

// Returns the auto sync status to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different format version).
// If GitHub refuses a read (for example, the token lacks the Actions permission), answers 502 with GitHub's message.
func handleSyncStatusGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	status, err := core.ReadSyncStatus(cfg)
	if err != nil {
		return err
	}
	core.WriteJSON(w, http.StatusOK, status)
	return nil
}

// Entry point for `/api/sync-status` requests.
// Vercel finds the exported function in this file and deploys it as one function.
// Accepts only GET. Other methods get 405.
func HandleSyncStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		core.WriteMethodNotAllowed(w)
		return
	}
	core.Respond(w, r, handleSyncStatusGet)
}
