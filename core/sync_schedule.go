// Schedule of auto sync.
// It checks scheduled calls (sync key), decides whether to sync that day, turns auto sync on and off, and reads the status shown in Settings.
// The scheduler is the workflow in the DB repository (`SyncWorkflowPath`).
// Every day it calls `/api/sync-archive` with the sync key, and `RunScheduledSync` decides whether to sync that day.
// The state a person chose (whether it is on, when it was turned on, the interval) is in `config.json`.
// The actual state (whether the workflow is on, the history of scheduled runs) isn't written by the server.
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

// Path in the DB repository of the workflow file that triggers auto sync.
// The setup tool installs it at this path, and the Actions API refers to the workflow by this file name
const SyncWorkflowPath = ".github/workflows/starkive-sync.yml"

// Name of the Actions secret in the DB repository that holds the sync key. The setup tool writes it under this name, and the workflow reads it by this name
const SyncKeySecretName = "STARKIVE_SYNC_KEY"

// Makes the key that checks scheduled sync calls: `deriveKey(token, "starkive-sync")` written in base64url.
//
// NOTE: The server stores this key nowhere.
// It computes it again from the token on each request and compares (a secret value can't be read back through the API, docs).
// The setup tool puts the same value into the secret of the DB repository.
// So if the way it is made changes, or the token changes, the setup tool must run again before scheduled calls work.
func DeriveSyncKey(cfg *Config) string {
	return base64.RawURLEncoding.EncodeToString(deriveKey(cfg, "starkive-sync"))
}

// 401 error for a missing or wrong sync key. This happens when the secret was deleted or the token changed.
// Answering with a failure makes the workflow fail, so GitHub notifies the user
var errSyncKeyRejected = Errorf(http.StatusUnauthorized, "Sync key missing or wrong. Run the starkive setup tool again")

// Checks a scheduled sync call and returns the server config.
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
// It is given only to workflow runs of that repository (docs), so only that workflow can make scheduled calls.
func AuthorizeScheduledSync(r *http.Request) (*Config, error) {
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

// Path that refers to the auto sync workflow in the Actions API (comes after `/repos/<repository>`).
// A workflow can be referred to by its file name instead of its ID (docs, measured).
// So there is no need to find or store an ID that differs per repository
var syncWorkflowAPIPath = "/actions/workflows/" + path.Base(SyncWorkflowPath)

// State of the auto sync workflow on GitHub (the fields in use from the answer to `GET .../actions/workflows/<file name>`)
type syncWorkflow struct {
	State string `json:"state"` // `active`, `disabled_manually` (turned off by a person), `disabled_inactivity` (public repository, 60 days without activity), and so on (docs)
}

// `state` value meaning the workflow is on. Every other value means it is off
const syncWorkflowActive = "active"

// Reads the state of the auto sync workflow from GitHub.
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

// One scheduled run (the fields in use from a GitHub Actions workflow run)
type SyncRun struct {
	StartedAt  time.Time `json:"run_started_at"` // Start time
	Conclusion string    `json:"conclusion"`     // Result. Anything other than `success` (`failure`, `cancelled`, `timed_out`, and so on) counts as a failure
	URL        string    `json:"html_url"`       // Address of the run's page on GitHub. Settings shows it as a link
}

// `conclusion` value meaning the scheduled run succeeded
const syncRunSucceeded = "success"

// Reads the last finished scheduled run from GitHub.
//
// Returns:
//   - The finished scheduled run that started last.
//     `nil` if there has never been one, or if there is no workflow (GitHub answers 404)
//
// NOTE: While a scheduled call is handled, the run that sent it is in the list, still in progress.
// If that run were picked as "the previous run", its result would be empty and yesterday's failure would be missed.
// So it is filtered out with `status=completed` (measured).
// Manual runs are filtered out with `event=schedule`.
// The docs don't promise a sort order for the list.
// So several runs are read, and the one with the latest start time is picked here.
func readLastScheduledRun(cfg *Config) (*SyncRun, error) {
	var list struct {
		Runs []SyncRun `json:"workflow_runs"`
	}
	err := callRepoAPI(cfg, http.MethodGet, syncWorkflowAPIPath+"/runs?event=schedule&status=completed&per_page=20", nil, &list)
	if hasGitHubStatus(err, http.StatusNotFound) { // If there is no workflow, its run list is 404 too
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var last *SyncRun
	for i := range list.Runs {
		if last == nil || list.Runs[i].StartedAt.After(last.StartedAt) {
			last = &list.Runs[i]
		}
	}
	return last, nil
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
//   - `intervalDays`: Sync interval (days). 1 or more
//   - `now`: Time to judge
//
// NOTE: It is decided by date math alone, so the last sync time doesn't need to be recorded.
// Sync now in the dashboard doesn't shift these days.
func isSyncDay(intervalDays int, now time.Time) bool {
	return syncDayNumber(now)%int64(intervalDays) == 0
}

// Returns the number of the sync day that `now` falls on: days since 1970-01-01, with each day starting at 06:00 UTC (`syncDayStart`)
func syncDayNumber(now time.Time) int64 {
	return int64(now.Add(-syncDayStart).Sub(time.Unix(0, 0)) / (24 * time.Hour))
}

// Handles a scheduled call (from the workflow in the DB repository).
// On a sync day it runs `Sync`, otherwise it does nothing.
// If the last finished scheduled run failed, it syncs again regardless of the interval.
//
// Args:
//   - `deadline`: Time after which the sync starts no new request of its long steps (passed to `Sync`)
//   - `now`: Current time. Taken as an argument so tests can set the date
//
// Returns:
//   - What it did: "not_due" (not a sync day) or "synced" (synced). The workflow counts only these two values as success
//   - The error if the config file or the run list couldn't be read, or if the sync failed
//
// NOTE: A failure isn't hidden as "not_due". It is returned as an error.
// The calling workflow must fail so that GitHub notifies the user.
// The on/off setting in the config file isn't checked.
// If the workflow runs, it syncs, and the page reports any mismatch with the setting.
// So a running workflow doesn't run for nothing just because the setting alone is off.
// If the return values change or new ones are added, also fix the workflow content of the setup tool (`syncWorkflowContent`) and raise the major version of `DBRepoFormat`.
// Overlapping runs aren't prevented.
// The workflow's `concurrency` runs scheduled runs one after another.
// Overlapping with Sync now gives the same sync result.
func RunScheduledSync(cfg *Config, deadline time.Time, now time.Time) (string, error) {
	settings, err := readSyncConfig(cfg)
	if err != nil {
		return "", err
	}
	last, err := readLastScheduledRun(cfg)
	if err != nil {
		return "", err
	}
	retry := last != nil && last.Conclusion != syncRunSucceeded
	if !retry && !isSyncDay(settings.intervalDays(), now) {
		return "not_due", nil
	}
	if _, err := Sync(cfg, deadline); err != nil {
		return "", err
	}
	return "synced", nil
}

// Changes auto sync from Settings: on/off and the interval.
// It changes only what is passed, and returns the state after the change.
// On/off turns the workflow itself in the DB repository on or off with the Actions API.
// It also writes the state the person chose (`enabled_since`) in the config file.
//
// Args:
//   - `enabled`: Turn on (`true`) or off (`false`). No change if `nil`
//   - `intervalDays`: New interval (days). No change if `nil`
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
			return SyncStatus{}, Errorf(http.StatusConflict, "Couldn't find the auto sync workflow in the archive repository. Run the starkive setup tool")
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
		if intervalDays != nil {
			file.Sync.IntervalDays = *intervalDays
			summaries = append(summaries, "sync every "+FormatCount(*intervalDays, "day"))
		}
		switch {
		case enabled == nil: // Don't change on/off
		case !*enabled:
			file.Sync.EnabledSince = nil
			summaries = append(summaries, "turn off auto sync")
		default:
			if file.Sync.EnabledSince == nil || !wasActive { // Write a new turn-on time, so the gap while it was off isn't seen as a stall
				file.Sync.markEnabledNow()
			}
			summaries = append(summaries, "turn on auto sync")
		}
		written = file.Sync // Set again if another commit comes in between and this is called again
		return NewCommitMessage("user(config)", strings.Join(summaries, ", ")), nil
	})
	if err != nil {
		return SyncStatus{}, err
	}

	status := SyncStatus{IntervalDays: written.intervalDays(), EnabledSince: written.EnabledSince, Workflow: state}
	if workflow != nil {
		if status.LastScheduledRun, err = readLastScheduledRun(cfg); err != nil {
			return SyncStatus{}, err
		}
	}
	return status, nil
}

// Writes in the config file that auto sync is on. If it is already on, it is left as is.
//
// NOTE: GitHub registers a workflow written through the API as on (measured).
// So the workflow isn't turned on separately, and only the setting is matched to that state.
func RecordSyncEnabled(cfg *Config) error {
	return updateConfig(cfg, func(file *dbConfigFile) (CommitMessage, error) {
		if file.Sync.EnabledSince == nil {
			file.Sync.markEnabledNow()
		}
		return NewCommitMessage("user(config)", "turn on auto sync"), nil
	})
}

// Sets the time auto sync was turned on to now (UTC, whole seconds).
// `(c *syncConfig)` is a pointer receiver, so the caller's value changes
func (c *syncConfig) markEnabledNow() {
	now := time.Now().UTC().Truncate(time.Second)
	c.EnabledSince = &now
}

// Auto sync status: the setting a person chose and the actual state on GitHub Actions.
// The page judges mismatches between setting and reality, stalls and failures from this value alone
type SyncStatus struct {
	IntervalDays     int        `json:"interval_days"`      // Auto sync interval (days)
	EnabledSince     *time.Time `json:"enabled_since"`      // Time a person turned auto sync on. The base for the stall check. Pointer: `nil` if the setting is off
	Workflow         string     `json:"workflow"`           // Actual workflow state: `missing` (none) or GitHub's `state` value (`active`, `disabled_manually`, `disabled_inactivity`, and so on)
	LastScheduledRun *SyncRun   `json:"last_scheduled_run"` // Last finished scheduled run. Pointer: `nil` if there has never been one
}

// `SyncStatus.Workflow` value meaning there is no workflow. It doesn't clash with GitHub's `state` values
const syncWorkflowMissing = "missing"

// Reads the auto sync setting and the actual state. It only reads (it makes no commit).
//
// Returns:
//   - The setting (interval, turn-on time) and the actual state (workflow state, last scheduled run).
//     If there is no workflow, `Workflow` is `missing` and the last scheduled run is empty
//   - An error if any of the three reads fails
//
// NOTE: The three questions to GitHub (config file, workflow state, run list) are sent at the same time, not one after another.
// So the answer takes as long as the slowest question, not the sum of all three.
// If there is no workflow, the run list request is wasted, but that is rare.
func ReadSyncStatus(cfg *Config) (SyncStatus, error) {
	var settings syncConfig
	var workflow *syncWorkflow
	var last *SyncRun
	errs := make([]error, 3)
	var wg sync.WaitGroup // Counter that waits until all started goroutines end (like `std::thread::join`)
	wg.Add(3)
	go func() { defer wg.Done(); settings, errs[0] = readSyncConfig(cfg) }() // Each goroutine writes a different variable, so writing without a lock doesn't overlap
	go func() { defer wg.Done(); workflow, errs[1] = readSyncWorkflow(cfg) }()
	go func() { defer wg.Done(); last, errs[2] = readLastScheduledRun(cfg) }()
	wg.Wait()
	if err := errors.Join(errs...); err != nil { // `errors.Join`: joins several errors into one. `nil` if all are `nil`
		return SyncStatus{}, err
	}
	status := SyncStatus{IntervalDays: settings.intervalDays(), EnabledSince: settings.EnabledSince, Workflow: syncWorkflowMissing}
	if workflow != nil {
		status.Workflow, status.LastScheduledRun = workflow.State, last
	}
	return status, nil
}
