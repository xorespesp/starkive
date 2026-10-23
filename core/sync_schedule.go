// When to sync: calls from the sync workflow and the schedule of auto sync.
// It checks calls from the sync workflow (sync key), decides whether to sync, turns sync on and off,
// and reads the status shown in Settings.
// Reading the workflow and its runs, and starting a run for Sync now, are in `sync_workflow.go`.
// The workflow in the DB repository (`SyncWorkflowPath`) runs every day on schedule, and when Sync now starts it.
// Each run calls `/api/sync-archive` with the sync key, and `SyncOnWorkflowCall` decides whether to sync.
// The state a person chose (whether it is on, when it was turned on, the auto sync interval) is in `config.json`.
// The actual state (whether the workflow is on, the history of runs) isn't written by the server.
// It is read from GitHub Actions with the Actions API.
// If the two disagree (changed in the Actions tab, or turned off by GitHub for inactivity), the page tells the person.
// The person then fixes it in Settings.
// The server never turns it on or off by itself.
// The sync itself (`SyncArchiveWithGitHub`) is in `sync.go`.
package core

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Name of the Actions secret in the DB repository that holds the sync key.
// The setup tool writes it under this name, and the workflow reads it by this name
const SyncKeySecretName = "STARKIVE_SYNC_KEY"

// Makes the key that checks calls from the sync workflow: `deriveKey(token, "starkive-sync")` written in base64url.
//
// NOTE: The server stores this key nowhere.
// It computes it again from the token on each request and compares (a secret value can't be read back through the API, docs).
// The setup tool puts the same value into the secret of the DB repository.
// So if the way it is made changes, or the token changes, the setup tool must run again before the workflow's calls work.
func DeriveSyncKey(cfg *EnvConfig) string {
	return base64.RawURLEncoding.EncodeToString(deriveKey(cfg, "starkive-sync"))
}

// 401 error for a missing or wrong sync key.
// This happens when the secret was deleted or the token changed.
// Answering with a failure makes the workflow fail, so GitHub notifies the user
var errSyncKeyRejected = Errorf(http.StatusUnauthorized, "Sync key missing or wrong. Run the starkive setup tool again")

// Checks a call from the sync workflow and opens the DB repository.
// It reads the environment variable, checks the sync key (`Authorization: Bearer <key>`),
// and opens the DB repository, which must be a starkive repository (`OpenDBRepo`).
//
// Returns:
//   - The opened DB repository.
//     A 401 `HTTPError` if the key is missing or wrong, a 409 if the repository can't be used
//
// NOTE: The key is checked with the environment variable alone, before asking GitHub.
// So repeated calls without the key don't use the server token's GitHub rate limit.
// No one can read the secret holding the key, even if the repository is public.
// It is given only to workflow runs of that repository (docs), so only that workflow can make these calls.
func AuthorizeSyncWorkflowCall(r *http.Request) (*DBRepo, error) {
	ctx := r.Context() // The deadline that `ServeAPIRequest` set for this request
	cfg, err := LoadEnvConfig()
	if err != nil {
		return nil, err
	}
	key, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || !hmac.Equal([]byte(key), []byte(DeriveSyncKey(cfg))) { // `hmac.Equal`: constant-time compare, so the time taken doesn't reveal the key
		return nil, errSyncKeyRejected
	}
	return OpenDBRepo(ctx, cfg)
}

// Day boundary used when counting sync days (06:00 UTC)
//
// NOTE: The workflow is scheduled at 18:17 UTC.
// GitHub delays or skips scheduled runs when it is busy (docs).
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
// A call from a run started by hand (Sync now) runs `SyncArchiveWithGitHub` right away, whatever the interval.
// It also uses the time left to refresh the saved READMEs whose turn to be refreshed hasn't come yet (`refreshReadmesEarly`).
// A call from a scheduled run runs `SyncArchiveWithGitHub` on a sync day, and otherwise does nothing.
// If the last finished scheduled run failed, a scheduled run syncs again whatever the interval, unless it is Manual only.
// With "Manual only" (`manualOnlyIntervalDays`), a scheduled run never syncs.
//
// Args:
//   - `ctx`: Deadline of the whole call. Every request ends by it
//   - `longStepsEndBy`: Time by which the long steps of the sync end (passed to `SyncArchiveWithGitHub`)
//   - `now`: Current time. Taken as an argument so tests can set the date
//
// Returns:
//   - What it did: `not_due` (not a sync day, or Manual only) or `synced`.
//     The workflow counts only these two values as success
//   - The error if the config file or the run list couldn't be read, or if the sync failed
//
// NOTE: A failure isn't hidden as `not_due`.
// It is returned as an error.
// The calling workflow must fail so that GitHub notifies the user.
// The on/off setting in the config file isn't checked.
// If the workflow runs, it syncs, and the page reports any mismatch with the setting.
// So a running workflow doesn't run for nothing just because the setting alone is off.
// If the return values change or new ones are added, also fix the workflow content of the setup tool (`syncWorkflowContent`) and raise the major version of `DBRepoFormat`.
// The call doesn't say which run sent it, and the installed workflow can't be changed to say it.
// So the run is found in the run list (`isCallFromManualWorkflowRun`).
// The workflow's `concurrency` runs one run at a time, so syncs don't overlap (docs, measured).
func SyncOnWorkflowCall(ctx context.Context, repo *DBRepo, longStepsEndBy time.Time, now time.Time) (string, error) {
	runList, err := readRecentSyncWorkflowRuns(ctx, repo, now)
	if err != nil {
		return "", err
	}
	isManualRun := isCallFromManualWorkflowRun(runList.pickRecentRuns(now))
	if !isManualRun {
		settings, err := readSyncConfig(ctx, repo)
		if err != nil {
			return "", err
		}
		intervalDays := settings.intervalDays()
		if intervalDays == manualOnlyIntervalDays {
			return "not_due", nil
		}
		last, err := findLastFinishedScheduledWorkflowRun(ctx, runList)
		if err != nil {
			return "", err
		}
		retry := last != nil && last.Conclusion != workflowRunSucceeded // A cancelled run is never `last` (`findLastFinishedScheduledWorkflowRun`)
		if !retry && !isAutoSyncDay(intervalDays, now) {
			return "not_due", nil
		}
	}
	if _, err := SyncArchiveWithGitHub(ctx, repo, longStepsEndBy, isManualRun); err != nil {
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
//     GitHub can briefly show a branch that just moved at its old commit (measured).
//     Reading again could then return the old setting, and the page would show the opposite.
//     What was written can be trusted.
//     `UpdateDBRepoFiles` reads the files at the commit by sha.
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
func UpdateSyncSettings(ctx context.Context, repo *DBRepo, enabled *bool, intervalDays *int) (SyncStatus, error) {
	if intervalDays != nil && !slices.Contains(syncIntervalChoices, *intervalDays) {
		return SyncStatus{}, Errorf(http.StatusBadRequest, "sync_interval_days must be one of %v", syncIntervalChoices)
	}
	workflow, err := readSyncWorkflow(ctx, repo)
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
			if err := callDBRepoAPI(ctx, repo, http.MethodPut, syncWorkflowAPIPath+action, nil, nil); err != nil {
				return SyncStatus{}, err
			}
			state = after // It shows up in the Actions API right away (measured), so don't ask again
		}
	}

	var written syncConfig // Setting this function wrote (or found nothing to change in). The answer is made from this value, not read again
	err = updateDBConfigFile(ctx, repo, func(file *dbConfigFile) (CommitMessage, error) {
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
		now := time.Now()
		runList, err := readRecentSyncWorkflowRuns(ctx, repo, now)
		if err != nil {
			return SyncStatus{}, err
		}
		recentRuns := runList.pickRecentRuns(now)
		status.ActiveRun, status.LastRun = findActiveWorkflowRun(recentRuns), findLastFinishedWorkflowRun(recentRuns)
		if status.LastScheduledRun, err = findLastFinishedScheduledWorkflowRun(ctx, runList); err != nil {
			return SyncStatus{}, err
		}
	}
	return status, nil
}

// Writes in the config file that sync is on.
// If it is already on, it is left as is.
//
// NOTE: GitHub registers a workflow written through the API as on (measured).
// So the workflow isn't turned on separately, and only the setting is matched to that state.
func RecordSyncEnabled(ctx context.Context, repo *DBRepo) error {
	return updateDBConfigFile(ctx, repo, func(file *dbConfigFile) (CommitMessage, error) {
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
	ActiveRun        *SyncWorkflowRun `json:"active_run"`         // Run of any kind that hasn't finished (`findActiveWorkflowRun`). Pointer: `nil` if no sync is running
	LastRun          *SyncWorkflowRun `json:"last_run"`           // Finished run of any kind that started last, since yesterday. Pointer: `nil` if there is none
}

// Reads the sync setting and the actual state.
// It only reads (it makes no commit).
//
// Returns:
//   - The setting (interval, turn-on time) and the actual state (workflow state, last scheduled run, running and last runs).
//     If there is no workflow, `Workflow` is `missing` and the runs are empty
//   - An error if any read fails
//
// NOTE: The three questions to GitHub (config file, workflow state, recent runs) are sent at the same time, not one after another.
// So the answer takes as long as the slowest question, not the sum of all three.
// The recent runs usually take one page of the run list (`readRecentSyncWorkflowRuns`).
// More pages are read only if they have no finished scheduled run (`findLastFinishedScheduledWorkflowRun`).
// If there is no workflow, the run list request is wasted, but that is rare.
func ReadSyncStatus(ctx context.Context, repo *DBRepo) (SyncStatus, error) {
	var settings syncConfig
	var workflow *syncWorkflow
	var runList *syncWorkflowRunList
	errs := make([]error, 3)
	now := time.Now()
	var wg sync.WaitGroup // Counter that waits until all started goroutines end (like `std::thread::join`)
	wg.Add(3)
	go func() { defer wg.Done(); settings, errs[0] = readSyncConfig(ctx, repo) }() // Each goroutine writes different variables, so writing without a lock doesn't overlap
	go func() { defer wg.Done(); workflow, errs[1] = readSyncWorkflow(ctx, repo) }()
	go func() { defer wg.Done(); runList, errs[2] = readRecentSyncWorkflowRuns(ctx, repo, now) }()
	wg.Wait()
	if err := errors.Join(errs...); err != nil { // `errors.Join`: joins several errors into one. `nil` if all are `nil`
		return SyncStatus{}, err
	}
	status := SyncStatus{IntervalDays: settings.intervalDays(), EnabledSince: settings.EnabledSince, Workflow: syncWorkflowMissing}
	if workflow != nil {
		recentRuns := runList.pickRecentRuns(now)
		status.ActiveRun, status.LastRun = findActiveWorkflowRun(recentRuns), findLastFinishedWorkflowRun(recentRuns)
		lastScheduled, err := findLastFinishedScheduledWorkflowRun(ctx, runList)
		if err != nil {
			return SyncStatus{}, err
		}
		status.Workflow, status.LastScheduledRun = workflow.State, lastScheduled
	}
	return status, nil
}
