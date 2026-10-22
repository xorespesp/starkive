// `/api/sync-status`: API that reads the sync status.
//
//	GET   -> `core.SyncStatus`: the sync setting in `config.json`, and the workflow state and runs on GitHub Actions
//	         (last scheduled run, the run that hasn't finished, the last finished run)
//
// Turning sync on and off, changing the auto sync interval and Sync now are in `/api/sync-archive`.

package handler

import (
	"net/http"

	"github.com/xorespesp/starkive/core"
)

// Returns the sync status to a signed-in user.
// Answers 401 if not signed in.
// Answers 409 if the DB repository can't be used (not set up yet, a foreign repository, or a different major format version).
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
