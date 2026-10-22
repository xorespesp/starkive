// `/api/sync-archive`: API for syncing the archive with GitHub (adding new Stars, refreshing the metadata and README snapshots) and for the sync settings.
//
//	GET              Call from the sync workflow. The DB repository's workflow (`starkive-sync.yml`) calls it every day on schedule,
//	                 and in each run started by hand, with the sync key in `Authorization: Bearer` -> { status }.
//	                 `status` is `not_due` (not a day to sync, or Manual only) or `synced` (synced). 401 if the key is missing or wrong.
//	                 A run started by hand syncs whatever the interval.
//	                 If the sync fails, it answers 502, so the workflow fails and GitHub notifies the user
//	POST             Sync now: a signed-in user starts a run of the sync workflow -> { run } (`core.SyncWorkflowRun`, queued).
//	                 It answers once the run exists, without waiting for the sync. 409 if the workflow is off or missing
//	PATCH            { enabled?, sync_interval_days? } -> The changed sync status (`core.SyncStatus`).
//	                 `enabled` turns the DB repository's workflow itself on or off,
//	                 and writes the state the person chose (`enabled_since`) to `config.json`.
//	                 `sync_interval_days` changes the auto sync interval in `config.json` (0 is Manual only)
//
// Only GET runs `core.Sync`, which does all the real sync work.

package handler

import (
	"log"
	"net/http"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Longest time one sync may take.
// The function time limit is 300 seconds (measured, with or without `maxDuration` in `vercel.json`).
// The workflow waits as long (`curl --max-time 300`).
// The 60 seconds left cover the work before and after the sync, and a last request that ends late
const syncTimeLimit = 240 * time.Second

// Handles a call from the sync workflow.
// Checks the sync key, syncs or not as `core.SyncOnWorkflowCall` decides, and answers `{ status }`.
// It syncs for a run started by hand, on a due day, or after a failed scheduled run.
// Answers 401 if the key is missing or wrong, 409 if the DB repository can't be used, and 502 if the sync fails.
//
// NOTE: The response has only the status, without counts or error details.
// The response body ends up in the workflow log, and an error text can contain repository names.
// If the DB repository is public, anyone can read that log.
// The details go only to the server log.
// The key is checked before anything is asked of GitHub.
// So even if someone calls this address over and over, it doesn't use the server token's rate limit.
func handleSyncArchiveGet(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.AuthorizeSyncWorkflowCall(r)
	if err != nil {
		return err
	}
	now := time.Now()
	status, err := core.SyncOnWorkflowCall(cfg, now.Add(syncTimeLimit), now)
	if err != nil {
		log.Printf("sync workflow call: %v", err)
		return core.Errorf(http.StatusBadGateway, "Sync failed. See the dashboard's server logs for details")
	}
	core.WriteJSON(w, http.StatusOK, map[string]string{"status": status})
	return nil
}

// Handles Sync now on the dashboard.
// Starts a run of the sync workflow (`core.TriggerSyncWorkflow`) and answers `{ run }` right away.
// The run calls GET like a scheduled run, and that call syncs whatever the interval.
// Only a signed-in user can call it.
//
// NOTE: The sync doesn't run in this request.
// So the page shows a running sync from the run list on GitHub (`active_run` of `/api/sync-status`),
// and that stays true after the page is closed or opened elsewhere.
func handleSyncArchivePost(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	run, err := core.TriggerSyncWorkflow(cfg)
	if err != nil {
		return err
	}
	answer := struct {
		Run core.SyncWorkflowRun `json:"run"` // The run that was started
	}{Run: run}
	core.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// Turns sync on or off, or changes the auto sync interval, and answers with the new status.
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
		SyncIntervalDays *int  `json:"sync_interval_days"` // New auto sync interval in days, 0 for Manual only. A pointer: if the field is missing, it doesn't change
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
