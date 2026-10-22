// The sync workflow and the schedule of auto sync.
// It checks calls from the sync workflow (sync key), decides whether to sync, turns sync on and off,
// starts a run of the workflow for Sync now, and reads the status shown in Settings.
// The workflow in the DB repository (`SyncWorkflowPath`) runs every day on schedule, and when Sync now starts it.
// Each run calls `/api/sync-archive` with the sync key, and `SyncOnWorkflowCall` decides whether to sync.
// The state a person chose (whether it is on, when it was turned on, the auto sync interval) is in `config.json`.
// The actual state (whether the workflow is on, the history of runs) isn't written by the server.
// It is read from GitHub Actions with the Actions API.
// If the two disagree (changed in the Actions tab, or turned off by GitHub for inactivity), the page tells the person.
// The person then fixes it in Settings.
// The server never turns it on or off by itself.
// The sync itself (`Sync`) is in `sync.go`.
package core

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
)

// Path in the DB repository of the sync workflow file.
// The setup tool installs it at this path, and the Actions API refers to the workflow by this file name
const SyncWorkflowPath = ".github/workflows/starkive-sync.yml"

// Name of the Actions secret in the DB repository that holds the sync key. The setup tool writes it under this name, and the workflow reads it by this name
const SyncKeySecretName = "STARKIVE_SYNC_KEY"

// Makes the key that checks calls from the sync workflow: `deriveKey(token, "starkive-sync")` written in base64url.
//
// NOTE: The server stores this key nowhere.
// It computes it again from the token on each request and compares (a secret value can't be read back through the API, docs).
// The setup tool puts the same value into the secret of the DB repository.
// So if the way it is made changes, or the token changes, the setup tool must run again before the workflow's calls work.
func DeriveSyncKey(cfg *Config) string {
	return base64.RawURLEncoding.EncodeToString(deriveKey(cfg, "starkive-sync"))
}

// 401 error for a missing or wrong sync key. This happens when the secret was deleted or the token changed.
// Answering with a failure makes the workflow fail, so GitHub notifies the user
var errSyncKeyRejected = Errorf(http.StatusUnauthorized, "Sync key missing or wrong. Run the starkive setup tool again")

// Checks a call from the sync workflow and returns the server config.
// It reads the environment variable, checks the sync key (`Authorization: Bearer <key>`),
// fills in the DB repository and checks that it is a starkive repository (`useDBRepo`).
//
// Returns:
//   - The server config with the DB repository filled in.
//     A 401 `HTTPError` if the key is missing or wrong, a 409 if the repository can't be used
//
// NOTE: The key is checked with the environment variable alone, before asking GitHub.
// So repeated calls without the key don't use the server token's GitHub rate limit.
// No one can read the secret holding the key, even if the repository is public.
// It is given only to workflow runs of that repository (docs), so only that workflow can make these calls.
func AuthorizeSyncWorkflowCall(r *http.Request) (*Config, error) {
	cfg, err := LoadEnvConfig()
	if err != nil {
		return nil, err
	}
	key, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || !hmac.Equal([]byte(key), []byte(DeriveSyncKey(cfg))) { // `hmac.Equal`: constant-time compare, so the time taken doesn't reveal the key
		return nil, errSyncKeyRejected
	}
	if err := useDBRepo(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Path that refers to the sync workflow in the Actions API (comes after `/repos/<repository>`).
// A workflow can be referred to by its file name instead of its ID (docs, measured).
// So there is no need to find or store an ID that differs per repository
var syncWorkflowAPIPath = "/actions/workflows/" + path.Base(SyncWorkflowPath)

// State of the sync workflow on GitHub (the fields in use from the answer to `GET .../actions/workflows/<file name>`)
type syncWorkflow struct {
	State string `json:"state"` // `active`, `disabled_manually` (turned off by a person), `disabled_inactivity` (public repository, 60 days without activity), and so on (docs)
}

// `state` value meaning the workflow is on. Every other value means it is off
const syncWorkflowActive = "active"

// Reads the state of the sync workflow from GitHub.
//
// Returns:
//   - The workflow state. `nil` if the DB repository has no workflow file (GitHub answers 404, measured)
func readSyncWorkflow(cfg *Config) (*syncWorkflow, error) {
	var workflow syncWorkflow
	err := callRepoAPI(cfg, http.MethodGet, syncWorkflowAPIPath, nil, &workflow)
	if hasGitHubStatus(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &workflow, nil
}

// One run of the sync workflow (the fields in use from a GitHub Actions workflow run)
type SyncWorkflowRun struct {
	ID         int64     `json:"id"`             // Run ID
	Event      string    `json:"event"`          // What started it: `schedule`, or `workflow_dispatch` (Sync now, or a person in the Actions tab)
	Status     string    `json:"status"`         // `queued`, `pending`, `in_progress`, `completed`, and so on (docs, measured)
	StartedAt  time.Time `json:"run_started_at"` // Start time
	Conclusion string    `json:"conclusion"`     // Result once `completed`: `success`, `failure`, `cancelled`, and so on. Empty before that
	URL        string    `json:"html_url"`       // Address of the run's page on GitHub. Settings shows it as a link
}

// Values of `SyncWorkflowRun` fields
const (
	workflowRunSucceeded   = "success"           // `Conclusion` of a run that succeeded
	workflowRunCancelled   = "cancelled"         // `Conclusion` of a run that GitHub or a person cancelled
	workflowRunCompleted   = "completed"         // `Status` of a finished run. Every other status means it hasn't finished
	workflowRunInProgress  = "in_progress"       // `Status` of a run whose job is running
	workflowRunManualEvent = "workflow_dispatch" // `Event` of a run started by hand (`TriggerSyncWorkflow`, or a person in the Actions tab)
)

// Number of recent days whose scheduled runs are read first when looking for the last run.
// A scheduled run comes once a day, so the scheduled runs of these days fit in one page of the list (20)
const lastScheduledRunWindowDays = 7

// Reads the last finished scheduled run that wasn't cancelled from GitHub.
// It reads the runs of the last `lastScheduledRunWindowDays` days first (the list's `created` filter, docs).
// Only if no such run is in those days, it reads the list without that filter.
//
// Args:
//   - `now`: Current time, which the window counts back from
//
// Returns:
//   - The finished scheduled run that started last.
//     `nil` if there has never been one, or if there is no workflow (GitHub answers 404)
//
// NOTE: While a call from the workflow is handled, the run that sent it is in the list, still in progress.
// If that run were picked as "the previous run", its result would be empty and yesterday's failure would be missed.
// So it is filtered out with `status=completed` (measured).
// Manual runs are filtered out with `event=schedule`.
// The docs don't promise a sort order for the list, so a page may not hold the newest run.
// The window keeps every recent run on one page, so the last one is found whatever the order.
// Without a run in the window, the result is the latest of the first page, which may not be the newest.
// That only happens when no scheduled run has finished for a week, and the dashboard warns about it anyway.
func readLastScheduledWorkflowRun(cfg *Config, now time.Time) (*SyncWorkflowRun, error) {
	since := now.UTC().AddDate(0, 0, -lastScheduledRunWindowDays).Format("2006-01-02") // Go writes a layout as one fixed example time (2006-01-02 15:04:05), so this is YYYY-MM-DD
	last, err := readLatestScheduledWorkflowRunOnPage(cfg, "&created=%3E%3D"+since)    // `created=>=<date>`, encoded
	if err != nil || last != nil {
		return last, err
	}
	return readLatestScheduledWorkflowRunOnPage(cfg, "")
}

// Reads one page (20) of the finished scheduled runs that match `filter`, and returns the one that started last.
// A cancelled run is skipped.
//
// Args:
//   - `filter`: More query parameters for the run list, starting with `&`. Empty for none
//
// Returns:
//   - The run. `nil` if the page has no run that wasn't cancelled, or if there is no workflow (GitHub answers 404)
func readLatestScheduledWorkflowRunOnPage(cfg *Config, filter string) (*SyncWorkflowRun, error) {
	runs, err := readSyncWorkflowRuns(cfg, "event=schedule&status=completed&per_page=20"+filter)
	if err != nil {
		return nil, err
	}
	var last *SyncWorkflowRun
	for i := range runs {
		// A cancelled run isn't a failed sync, so it says nothing about the sync and the run before it counts.
		// For example, `concurrency` cancels a run that waits behind another when a third one comes (docs)
		if runs[i].Conclusion == workflowRunCancelled {
			continue
		}
		if last == nil || runs[i].StartedAt.After(last.StartedAt) {
			last = &runs[i]
		}
	}
	return last, nil
}

// Reads one page of the sync workflow's run list.
//
// Args:
//   - `query`: Query parameters of the run list (e.g. `event=schedule&per_page=20`)
//
// Returns:
//   - The runs on the page, in GitHub's order. Empty if there is no workflow (GitHub answers 404)
func readSyncWorkflowRuns(cfg *Config, query string) ([]SyncWorkflowRun, error) {
	var list struct {
		Runs []SyncWorkflowRun `json:"workflow_runs"`
	}
	err := callRepoAPI(cfg, http.MethodGet, syncWorkflowAPIPath+"/runs?"+query, nil, &list)
	if hasGitHubStatus(err, http.StatusNotFound) { // If there is no workflow, its run list is 404 too
		return []SyncWorkflowRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Runs, nil
}

// Reads the sync workflow runs of any kind started since yesterday (UTC), with one request.
//
// Args:
//   - `now`: Current time
//
// Returns:
//   - The run that hasn't finished, preferring one whose job is running. `nil` if every run has finished
//   - The finished run that started last. `nil` if none finished since yesterday
//
// NOTE: The workflow's `concurrency` lets one run go at a time, and keeps one more waiting (docs, measured).
// With the scheduled run once a day and Sync now locked while a run goes, the runs since yesterday normally fit in one page (20).
// So both are found whatever the order of the list.
func readRecentSyncWorkflowRuns(cfg *Config, now time.Time) (*SyncWorkflowRun, *SyncWorkflowRun, error) {
	since := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")                  // YYYY-MM-DD (the layout is Go's example time)
	runs, err := readSyncWorkflowRuns(cfg, "per_page=20&created=%3E%3D"+since) // `created=>=<date>`, encoded
	if err != nil {
		return nil, nil, err
	}
	var unfinished *SyncWorkflowRun
	var lastFinished *SyncWorkflowRun
	for i := range runs {
		run := &runs[i]
		switch {
		case run.Status != workflowRunCompleted:
			if unfinished == nil || run.Status == workflowRunInProgress {
				unfinished = run
			}
		case lastFinished == nil || run.StartedAt.After(lastFinished.StartedAt):
			lastFinished = run
		}
	}
	return unfinished, lastFinished, nil
}

// Reports whether a run started by hand (`workflow_dispatch`) has its job running now.
//
// NOTE: While a call from the workflow is handled, the run that sent it is in the list as `in_progress` (measured).
// The workflow's `concurrency` lets only one run go at a time (docs, measured).
// So a manual run that is `in_progress` is the run that sent the call.
func isManualWorkflowRunInProgress(cfg *Config) (bool, error) {
	runs, err := readSyncWorkflowRuns(cfg, "event="+workflowRunManualEvent+"&status="+workflowRunInProgress+"&per_page=1")
	if err != nil {
		return false, err
	}
	return len(runs) > 0, nil
}

// Starts a run of the sync workflow now (Sync now), and returns it.
//
// Returns:
//   - The run, queued.
//     A 409 `HTTPError` if there is no workflow or it is off. The `GitHubAPIError` if GitHub refuses
//
// NOTE: GitHub refuses to start a workflow that is off, with 422 (measured).
// So the state is read first, and the answer says what to do.
// With `return_run_details`, GitHub answers after the run exists, with its ID and page (measured).
// The run showed in the run list half a second after the answer (measured).
func TriggerSyncWorkflow(cfg *Config) (SyncWorkflowRun, error) {
	workflow, err := readSyncWorkflow(cfg)
	if err != nil {
		return SyncWorkflowRun{}, err
	}
	if workflow == nil {
		return SyncWorkflowRun{}, Errorf(http.StatusConflict, "Couldn't find the sync workflow in the archive repository. Run the starkive setup tool")
	}
	if workflow.State != syncWorkflowActive {
		return SyncWorkflowRun{}, Errorf(http.StatusConflict, "The sync workflow is off. Turn it on in Settings to use Sync now")
	}
	var started struct {
		ID  int64  `json:"workflow_run_id"`
		URL string `json:"html_url"`
	}
	body := map[string]any{"ref": cfg.DBRepoBranch, "return_run_details": true}
	if err := callRepoAPI(cfg, http.MethodPost, syncWorkflowAPIPath+"/dispatches", body, &started); err != nil {
		return SyncWorkflowRun{}, err
	}
	return SyncWorkflowRun{ID: started.ID, Event: workflowRunManualEvent, Status: "queued", URL: started.URL}, nil
}

// Day boundary used when counting sync days (06:00 UTC)
//
// NOTE: The workflow is scheduled at 18:17 UTC.
// GitHub delays or skips scheduled runs when it is busy.
// With the boundary at midnight, a run only 5 hours 43 minutes late would count as the next day.
// So the boundary is 06:00, the time farthest from the schedule.
const syncDayStart = 6 * time.Hour

// Reports whether `now` falls on a day that fits the interval.
// A sync day is a day whose count from 1970-01-01 is a multiple of the interval (every day if the interval is 1).
//
// Args:
//   - `intervalDays`: Auto sync interval (days). 1 or more
//   - `now`: Time to judge
//
// NOTE: It is decided by date math alone, so the last sync time doesn't need to be recorded.
// Sync now in the dashboard doesn't shift these days.
func isAutoSyncDay(intervalDays int, now time.Time) bool {
	return syncDayNumber(now)%int64(intervalDays) == 0
}

// Returns the number of the sync day that `now` falls on: days since 1970-01-01, with each day starting at 06:00 UTC (`syncDayStart`)
func syncDayNumber(now time.Time) int64 {
	return int64(now.Add(-syncDayStart).Sub(time.Unix(0, 0)) / (24 * time.Hour))
}

// Handles a call from the sync workflow in the DB repository.
// A call from a run started by hand (Sync now) runs `Sync` right away, whatever the interval.
// A call from a scheduled run runs `Sync` on a sync day, and otherwise does nothing.
// If the last finished scheduled run failed, a scheduled run syncs again regardless of the interval.
// With "Manual only" (`manualOnlyIntervalDays`), a scheduled run never syncs.
//
// Args:
//   - `deadline`: Time after which the sync starts no new request of its long steps (passed to `Sync`)
//   - `now`: Current time. Taken as an argument so tests can set the date
//
// Returns:
//   - What it did: "not_due" (not a sync day, or Manual only) or "synced" (synced). The workflow counts only these two values as success
//   - The error if the config file or the run list couldn't be read, or if the sync failed
//
// NOTE: A failure isn't hidden as "not_due". It is returned as an error.
// The calling workflow must fail so that GitHub notifies the user.
// The on/off setting in the config file isn't checked.
// If the workflow runs, it syncs, and the page reports any mismatch with the setting.
// So a running workflow doesn't run for nothing just because the setting alone is off.
// If the return values change or new ones are added, also fix the workflow content of the setup tool (`syncWorkflowContent`) and raise the major version of `DBRepoFormat`.
// The call doesn't say which run sent it, and the installed workflow can't be changed to say it.
// So the run is found in the run list (`isManualWorkflowRunInProgress`).
// The workflow's `concurrency` runs one run at a time, so syncs don't overlap.
func SyncOnWorkflowCall(cfg *Config, deadline time.Time, now time.Time) (string, error) {
	isManual, err := isManualWorkflowRunInProgress(cfg)
	if err != nil {
		return "", err
	}
	if !isManual {
		settings, err := readSyncConfig(cfg)
		if err != nil {
			return "", err
		}
		intervalDays := settings.intervalDays()
		if intervalDays == manualOnlyIntervalDays {
			return "not_due", nil
		}
		last, err := readLastScheduledWorkflowRun(cfg, now)
		if err != nil {
			return "", err
		}
		retry := last != nil && last.Conclusion != workflowRunSucceeded // A cancelled run is never `last` (`readLatestScheduledWorkflowRunOnPage`)
		if !retry && !isAutoSyncDay(intervalDays, now) {
			return "not_due", nil
		}
	}
	if _, err := Sync(cfg, deadline); err != nil {
		return "", err
	}
	return "synced", nil
}

// Changes sync from Settings: on/off and the auto sync interval.
// It changes only what is passed, and returns the state after the change.
// On/off turns the workflow itself in the DB repository on or off with the Actions API.
// It also writes the state the person chose (`enabled_since`) in the config file.
//
// Args:
//   - `enabled`: Turn on (`true`) or off (`false`). No change if `nil`
//   - `intervalDays`: New auto sync interval (days), or `manualOnlyIntervalDays`. No change if `nil`
//
// Returns:
//   - The state after the change. The setting values are what this function wrote. They aren't read again
//   - A 400 `HTTPError` for an interval that isn't a choice.
//     A 409 `HTTPError` asking to run the setup tool, if there is no workflow to turn on or off.
//     The `GitHubAPIError` if GitHub refuses
//
// NOTE:
//   - The state is made from what was written, not read again.
//     GitHub can briefly show a branch that just moved at its old commit.
//     Reading again could then return the old setting, and the page would show the opposite.
//     What was written can be trusted.
//     `UpdateFiles` reads the files at the commit by sha.
//     A commit made on a stale commit is refused when the branch moves, and the work starts over.
//     So what a successful write saw is the latest content.
//   - The two values are written in one commit.
//     So there is no state where one is saved and the other failed.
//     The workflow is changed before that.
//     If writing the setting fails, only the workflow stays changed, and the page reports the mismatch.
//   - Turning off a workflow that is already off makes GitHub refuse with a 403 that has the same header as a missing permission (`actions=write`) (measured).
//     So the state is read first, and the call is made only when it changes.
//     Turning on or off shows up in the Actions API right away (measured).
//     So the state after the change is set without asking again.
//   - When turning on a workflow that was off, the time it was turned on is written again as now.
//     There were no scheduled runs while it was off.
//     This keeps the page's stall check from seeing that gap as a stall.
//     If both the setting and the workflow are already on, nothing changes and no commit is made.
//   - A workflow that GitHub turned off for inactivity (`disabled_inactivity`) is turned on the same way.
//     It is turned on only when a person clicks, never by itself
//     (getting around the inactivity rule automatically breaks GitHub's terms).
func UpdateSyncSettings(cfg *Config, enabled *bool, intervalDays *int) (SyncStatus, error) {
	if intervalDays != nil && !slices.Contains(syncIntervalChoices, *intervalDays) {
		return SyncStatus{}, Errorf(http.StatusBadRequest, "sync_interval_days must be one of %v", syncIntervalChoices)
	}
	workflow, err := readSyncWorkflow(cfg)
	if err != nil {
		return SyncStatus{}, err
	}
	state := syncWorkflowMissing
	if workflow != nil {
		state = workflow.State
	}
	wasActive := state == syncWorkflowActive
	if enabled != nil {
		if workflow == nil {
			return SyncStatus{}, Errorf(http.StatusConflict, "Couldn't find the sync workflow in the archive repository. Run the starkive setup tool")
		}
		if wasActive != *enabled { // GitHub refuses with 403 when turning off what is already off, so call only on a change
			action, after := "/disable", "disabled_manually"
			if *enabled {
				action, after = "/enable", syncWorkflowActive
			}
			if err := callRepoAPI(cfg, http.MethodPut, syncWorkflowAPIPath+action, nil, nil); err != nil {
				return SyncStatus{}, err
			}
			state = after // It shows up in the Actions API right away (measured), so don't ask again
		}
	}

	var written syncConfig // Setting this function wrote (or found nothing to change in). The answer is made from this value, not read again
	err = updateConfig(cfg, func(file *dbConfigFile) (CommitMessage, error) {
		summaries := []string{}
		switch {
		case intervalDays == nil: // Don't change the interval
		case *intervalDays == manualOnlyIntervalDays:
			file.Sync.IntervalDays = intervalDays
			summaries = append(summaries, "sync manually only")
		default:
			file.Sync.IntervalDays = intervalDays
			summaries = append(summaries, "sync every "+FormatCount(*intervalDays, "day"))
		}
		switch {
		case enabled == nil: // Don't change on/off
		case !*enabled:
			file.Sync.EnabledSince = nil
			summaries = append(summaries, "turn off sync")
		default:
			if file.Sync.EnabledSince == nil || !wasActive { // Write a new turn-on time, so the gap while it was off isn't seen as a stall
				file.Sync.markEnabledNow()
			}
			summaries = append(summaries, "turn on sync")
		}
		written = file.Sync // Set again if another commit comes in between and this is called again
		return NewCommitMessage("user(config)", strings.Join(summaries, ", ")), nil
	})
	if err != nil {
		return SyncStatus{}, err
	}

	status := SyncStatus{IntervalDays: written.intervalDays(), EnabledSince: written.EnabledSince, Workflow: state}
	if workflow != nil {
		if status.LastScheduledRun, err = readLastScheduledWorkflowRun(cfg, time.Now()); err != nil {
			return SyncStatus{}, err
		}
		if status.ActiveRun, status.LastRun, err = readRecentSyncWorkflowRuns(cfg, time.Now()); err != nil {
			return SyncStatus{}, err
		}
	}
	return status, nil
}

// Writes in the config file that sync is on. If it is already on, it is left as is.
//
// NOTE: GitHub registers a workflow written through the API as on (measured).
// So the workflow isn't turned on separately, and only the setting is matched to that state.
func RecordSyncEnabled(cfg *Config) error {
	return updateConfig(cfg, func(file *dbConfigFile) (CommitMessage, error) {
		if file.Sync.EnabledSince == nil {
			file.Sync.markEnabledNow()
		}
		return NewCommitMessage("user(config)", "turn on sync"), nil
	})
}

// Sets the time sync was turned on to now (UTC, whole seconds).
// `(c *syncConfig)` is a pointer receiver, so the caller's value changes
func (c *syncConfig) markEnabledNow() {
	now := time.Now().UTC().Truncate(time.Second)
	c.EnabledSince = &now
}

// Sync status: the setting a person chose and the actual state on GitHub Actions.
// The page judges mismatches between setting and reality, stalls, failures and whether a sync is running from this value alone
type SyncStatus struct {
	IntervalDays     int              `json:"interval_days"`      // Auto sync interval (days), or `manualOnlyIntervalDays`
	EnabledSince     *time.Time       `json:"enabled_since"`      // Time a person turned sync on. The base for the stall check. Pointer: `nil` if the setting is off
	Workflow         string           `json:"workflow"`           // Actual workflow state: `missing` (none) or GitHub's `state` value (`active`, `disabled_manually`, `disabled_inactivity`, and so on)
	LastScheduledRun *SyncWorkflowRun `json:"last_scheduled_run"` // Last finished scheduled run that wasn't cancelled. Pointer: `nil` if there has never been one
	ActiveRun        *SyncWorkflowRun `json:"active_run"`         // Run of any kind that hasn't finished (`readRecentSyncWorkflowRuns`). Pointer: `nil` if no sync is running
	LastRun          *SyncWorkflowRun `json:"last_run"`           // Finished run of any kind that started last, since yesterday. Pointer: `nil` if there is none
}

// `SyncStatus.Workflow` value meaning there is no workflow. It doesn't clash with GitHub's `state` values
const syncWorkflowMissing = "missing"

// Reads the sync setting and the actual state. It only reads (it makes no commit).
//
// Returns:
//   - The setting (interval, turn-on time) and the actual state (workflow state, last scheduled run, running and last runs).
//     If there is no workflow, `Workflow` is `missing` and the runs are empty
//   - An error if any of the four reads fails
//
// NOTE: The four questions to GitHub (config file, workflow state, two run lists) are sent at the same time, not one after another.
// So the answer takes as long as the slowest question, not the sum of all four.
// If there is no workflow, the run list requests are wasted, but that is rare.
func ReadSyncStatus(cfg *Config) (SyncStatus, error) {
	var settings syncConfig
	var workflow *syncWorkflow
	var lastScheduled, active, last *SyncWorkflowRun
	errs := make([]error, 4)
	now := time.Now()
	var wg sync.WaitGroup // Counter that waits until all started goroutines end (like `std::thread::join`)
	wg.Add(4)
	go func() { defer wg.Done(); settings, errs[0] = readSyncConfig(cfg) }() // Each goroutine writes different variables, so writing without a lock doesn't overlap
	go func() { defer wg.Done(); workflow, errs[1] = readSyncWorkflow(cfg) }()
	go func() { defer wg.Done(); lastScheduled, errs[2] = readLastScheduledWorkflowRun(cfg, now) }()
	go func() { defer wg.Done(); active, last, errs[3] = readRecentSyncWorkflowRuns(cfg, now) }()
	wg.Wait()
	if err := errors.Join(errs...); err != nil { // `errors.Join`: joins several errors into one. `nil` if all are `nil`
		return SyncStatus{}, err
	}
	status := SyncStatus{IntervalDays: settings.intervalDays(), EnabledSince: settings.EnabledSince, Workflow: syncWorkflowMissing}
	if workflow != nil {
		status.Workflow, status.LastScheduledRun, status.ActiveRun, status.LastRun = workflow.State, lastScheduled, active, last
	}
	return status, nil
}
