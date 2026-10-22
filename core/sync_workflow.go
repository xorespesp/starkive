// The sync workflow in the DB repository (`SyncWorkflowPath`), read and started through the GitHub Actions API.
// It reads whether the workflow is on (`readSyncWorkflow`) and its runs (`SyncWorkflowRun`),
// tells which kind of run sent a call (`isCallFromManualWorkflowRun`), and starts a run for Sync now (`TriggerSyncWorkflow`).
// When to sync, the sync key and the sync settings are in `sync_schedule.go`.
package core

import (
	"context"
	"net/http"
	"path"
	"time"
)

// Path in the DB repository of the sync workflow file.
// The setup tool installs it at this path, and the Actions API refers to the workflow by this file name
const SyncWorkflowPath = ".github/workflows/starkive-sync.yml"

// Path that refers to the sync workflow in the Actions API (comes after `/repos/<repository>`).
// A workflow can be referred to by its file name instead of its ID (docs, measured).
// So there is no need to find or store an ID that differs per repository
var syncWorkflowAPIPath = "/actions/workflows/" + path.Base(SyncWorkflowPath)

// State of the sync workflow on GitHub (the fields in use from the answer to `GET .../actions/workflows/<file name>`)
type syncWorkflow struct {
	State string `json:"state"` // `active`, `disabled_manually` (turned off by a person), `disabled_inactivity` (public repository, 60 days without activity), and so on (docs)
}

// `state` value meaning the workflow is on.
// Every other value means it is off
const syncWorkflowActive = "active"

// Reads the state of the sync workflow from GitHub.
//
// Returns:
//   - The workflow state. `nil` if the DB repository has no workflow file (GitHub answers 404, measured)
func readSyncWorkflow(ctx context.Context, repo *DBRepo) (*syncWorkflow, error) {
	var workflow syncWorkflow
	err := callDBRepoAPI(ctx, repo, http.MethodGet, syncWorkflowAPIPath, nil, &workflow)
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
	URL        string    `json:"html_url"`       // Address of the run's page on GitHub
}

// Values of `SyncWorkflowRun` fields
const (
	workflowRunSucceeded   = "success"           // `Conclusion` of a run that succeeded
	workflowRunCancelled   = "cancelled"         // `Conclusion` of a run that GitHub or a person cancelled
	workflowRunCompleted   = "completed"         // `Status` of a finished run. Every other status means it hasn't finished
	workflowRunInProgress  = "in_progress"       // `Status` of a run whose job is running
	workflowRunQueued      = "queued"            // `Status` of a run waiting for a runner
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
// That only happens when no scheduled run has finished without being cancelled for a week, and the dashboard warns about it anyway.
func readLastScheduledWorkflowRun(ctx context.Context, repo *DBRepo, now time.Time) (*SyncWorkflowRun, error) {
	since := now.UTC().AddDate(0, 0, -lastScheduledRunWindowDays).Format("2006-01-02")  // Go writes a layout as one fixed example time (2006-01-02 15:04:05), so this is YYYY-MM-DD
	last, err := readLastScheduledWorkflowRunOnPage(ctx, repo, "&created=%3E%3D"+since) // `created=>=<date>`, encoded
	if err != nil || last != nil {
		return last, err
	}
	return readLastScheduledWorkflowRunOnPage(ctx, repo, "")
}

// Reads one page (20) of the finished scheduled runs that match `filter`, and returns the one that started last.
// A cancelled run is skipped.
//
// Args:
//   - `filter`: More query parameters for the run list, starting with `&`. Empty for none
//
// Returns:
//   - The run. `nil` if the page has no run that wasn't cancelled, or if there is no workflow (GitHub answers 404)
func readLastScheduledWorkflowRunOnPage(ctx context.Context, repo *DBRepo, filter string) (*SyncWorkflowRun, error) {
	runs, err := readSyncWorkflowRuns(ctx, repo, "event=schedule&status=completed&per_page=20"+filter)
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
func readSyncWorkflowRuns(ctx context.Context, repo *DBRepo, query string) ([]SyncWorkflowRun, error) {
	var list struct {
		Runs []SyncWorkflowRun `json:"workflow_runs"`
	}
	err := callDBRepoAPI(ctx, repo, http.MethodGet, syncWorkflowAPIPath+"/runs?"+query, nil, &list)
	if hasGitHubStatus(err, http.StatusNotFound) { // If there is no workflow, its run list is 404 too
		return []SyncWorkflowRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Runs, nil
}

// Reads the sync workflow runs of any kind created since the day before `now` (UTC), with one request.
//
// Returns:
//   - The run that hasn't finished, preferring one whose job is running. `nil` if every run has finished
//   - The finished run that started last. `nil` if none finished since yesterday
//
// NOTE: The workflow's `concurrency` lets one run go at a time, and keeps one more waiting (docs, measured).
// With the scheduled run once a day and Sync now locked while a run goes, the runs since yesterday normally fit in one page (20).
// So both are found whatever the order of the list.
func readRecentSyncWorkflowRuns(ctx context.Context, repo *DBRepo, now time.Time) (*SyncWorkflowRun, *SyncWorkflowRun, error) {
	since := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")                        // YYYY-MM-DD (the layout is Go's example time)
	runs, err := readSyncWorkflowRuns(ctx, repo, "per_page=20&created=%3E%3D"+since) // `created=>=<date>`, encoded
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

// Reports whether the run that sent the current call was started by hand (`workflow_dispatch`).
//
// Args:
//   - `now`: Current time, for the runs since yesterday (`readRecentSyncWorkflowRuns`)
//
// NOTE: The call doesn't say which run sent it, and the installed workflow can't be changed to say it.
// While a call from the workflow is handled, the run that sent it is in the list as `in_progress` (measured).
// The workflow's `concurrency` lets only one run go at a time (docs, measured).
// So the run that is `in_progress` is the run that sent the call.
// If the list doesn't show any run as `in_progress` yet, a manual run that hasn't finished counts.
// Then a Sync now isn't taken for a scheduled run that isn't due, and the worst case is one extra sync.
func isCallFromManualWorkflowRun(ctx context.Context, repo *DBRepo, now time.Time) (bool, error) {
	since := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")                        // YYYY-MM-DD (the layout is Go's example time)
	runs, err := readSyncWorkflowRuns(ctx, repo, "per_page=20&created=%3E%3D"+since) // `created=>=<date>`, encoded
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if run.Status == workflowRunInProgress {
			return run.Event == workflowRunManualEvent, nil
		}
	}
	for _, run := range runs {
		if run.Status != workflowRunCompleted && run.Event == workflowRunManualEvent {
			return true, nil
		}
	}
	return false, nil
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
func TriggerSyncWorkflow(ctx context.Context, repo *DBRepo) (SyncWorkflowRun, error) {
	workflow, err := readSyncWorkflow(ctx, repo)
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
	body := map[string]any{"ref": repo.branch, "return_run_details": true}
	if err := callDBRepoAPI(ctx, repo, http.MethodPost, syncWorkflowAPIPath+"/dispatches", body, &started); err != nil {
		return SyncWorkflowRun{}, err
	}
	return SyncWorkflowRun{ID: started.ID, Event: workflowRunManualEvent, Status: workflowRunQueued, URL: started.URL}, nil
}

// `SyncStatus.Workflow` value meaning there is no workflow.
// It doesn't clash with GitHub's `state` values
const syncWorkflowMissing = "missing"
