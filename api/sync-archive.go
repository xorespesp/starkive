// `/api/sync-archive`: API for syncing the archive with GitHub (adding new Stars, refreshing the metadata snapshot) and for the auto sync settings.
//
//	GET              Scheduled call. The DB repository's workflow (`starkive-sync.yml`) calls it every day,
//	                 with the sync key in `Authorization: Bearer` -> { status }.
//	                 `status` is `not_due` (not a day to sync) or `synced` (synced). 401 if the key is missing or wrong.
//	                 If the sync fails, it answers 502, so the workflow fails and GitHub notifies the user
//	POST             Sync now: a signed-in user syncs right away, whatever the interval -> { starred, added, meta_checked, unavailable }
//	PATCH            { enabled?, sync_interval_days? } -> The changed auto sync status (`core.SyncStatus`).
//	                 `enabled` turns the DB repository's workflow itself on or off,
//	                 and writes the state the person chose (`enabled_since`) to `config.json`.
//	                 `sync_interval_days` changes the interval in `config.json`
//
// `core.Sync` does all the real sync work.

package handler

import (
	"log"
	"net/http"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Time one sync may spend refreshing the metadata snapshot.
// It is well below the time limit of this function (300 seconds, `maxDuration` in `vercel.json`).
// A request for 100 repositories takes about 4.5 seconds (measured), and 6 are sent at once.
// So this is enough to check thousands at a time. Repositories not checked yet get checked in the next run
const metaRefreshBudget = 40 * time.Second

// Handles the workflow's scheduled call.
// Checks the sync key, syncs if today is a due day, and answers `{ status }`.
// Answers 401 if the key is missing or wrong, 409 if the DB repository can't be used, and 502 if the sync fails.
//
// NOTE: The response has only the status, without counts or error details.
// The response body ends up in the workflow log, and an error text can contain repository names.
// If the DB repository is public, anyone can read that log.
// The details go only to the server log.
// The key is checked before anything is asked of GitHub.
// So even if someone calls this address over and over, it doesn't use the server token's rate limit.
func handleSyncArchiveGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.AuthorizeScheduledSync(r)
	if err != nil {
		return err
	}
	status, err := core.RunScheduledSync(cfg, metaRefreshBudget, time.Now())
	if err != nil {
		log.Printf("scheduled sync: %v", err)
		return core.Errorf(http.StatusBadGateway, "Sync failed. See the dashboard's server logs for details")
	}
	core.WriteJSON(w, http.StatusOK, map[string]string{"status": status})
	return nil
}

// Handles Sync now on the dashboard.
// Syncs right away, whatever the interval, and answers with the counts (`core.SyncResult`).
// Only a signed-in user can call it.
// If the sync fails, it returns that error as is (the person who clicked sees it right away).
func handleSyncArchivePost(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	result, err := core.Sync(cfg, metaRefreshBudget)
	if err != nil {
		return err
	}
	core.WriteJSON(w, http.StatusOK, result)
	return nil
}

// Turns auto sync on or off, or changes the interval, and answers with the new status.
// Only the fields that were sent change. If both are sent, they are written in one commit (`core.UpdateSyncSettings`).
// Answers 400 if neither field is sent or the interval isn't one of the choices.
// Answers 409 when turning it on or off and the DB repository has no workflow.
func handleSyncArchivePatch(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	var body struct {
		Enabled          *bool `json:"enabled"`            // Turn on (`true`) or off (`false`). A pointer: if the field is missing (`nil`), it doesn't change
		SyncIntervalDays *int  `json:"sync_interval_days"` // New interval in days. A pointer: if the field is missing, it doesn't change
	}
	if err := core.ReadJSON(r, &body); err != nil {
		return err
	}
	if body.Enabled == nil && body.SyncIntervalDays == nil {
		return core.Errorf(http.StatusBadRequest, "Send enabled or sync_interval_days")
	}
	status, err := core.UpdateSyncSettings(cfg, body.Enabled, body.SyncIntervalDays)
	if err != nil {
		return err
	}
	core.WriteJSON(w, http.StatusOK, status)
	return nil
}

// Entry point for `/api/sync-archive` requests. It sends each method to its handler.
// Vercel finds the exported function in this file and deploys it as one function.
// `core.Respond` turns an error from a handler into a JSON error response.
// Other methods get 405.
func HandleSyncArchive(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		core.Respond(w, r, handleSyncArchiveGet)
	case http.MethodPost:
		core.Respond(w, r, handleSyncArchivePost)
	case http.MethodPatch:
		core.Respond(w, r, handleSyncArchivePatch)
	default:
		core.WriteMethodNotAllowed(w)
	}
}
