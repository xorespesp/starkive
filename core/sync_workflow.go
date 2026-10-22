// The sync workflow in the DB repository (`SyncWorkflowPath`), read and started through the GitHub Actions API.
// It reads whether the workflow is on (`readSyncWorkflow`) and its runs (`SyncWorkflowRun`),
// tells which kind of run sent a call (`isCallFromManualWorkflowRun`), and starts a run for Sync now (`TriggerSyncWorkflow`).
// When to sync, the sync key and the sync settings are in `sync_schedule.go`.
package core

import (
	"context"
	"fmt"
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
	workflowRunSucceeded      = "success"           // `Conclusion` of a run that succeeded
	workflowRunCancelled      = "cancelled"         // `Conclusion` of a run that GitHub or a person cancelled
	workflowRunCompleted      = "completed"         // `Status` of a finished run. Every other status means it hasn't finished
	workflowRunInProgress     = "in_progress"       // `Status` of a run whose job is running
	workflowRunQueued         = "queued"            // `Status` of a run waiting for a runner
	workflowRunManualEvent    = "workflow_dispatch" // `Event` of a run started by hand (`TriggerSyncWorkflow`, or a person in the Actions tab)
	workflowRunScheduledEvent = "schedule"          // `Event` of a scheduled run
)

// Runs in one page of the run list.
// One run takes about 13KB in the answer (measured), so a page is about 260KB.
// With the scheduled run once a day, the first page usually reaches past the recent runs (`isRecentWorkflowRun`),
// so reading them takes one request
const syncWorkflowRunsPerPage = 20

// Pages of the run list, counted from page 1, that looking for the last scheduled run reads up to (100 runs).
// If reading the recent runs already took more pages, it reads no more.
// A scheduled run comes once a day, so it is on the first page unless many manual runs came after it
const maxScheduledRunPages = 5

// Reads one page of the sync workflow's run list, newest first (measured).
//
// Args:
//   - `page`: Page number, starting at 1
//
// Returns:
//   - The runs on the page. Empty if there is no workflow (GitHub answers 404)
//
// NOTE: The list is read without filters, so the page holds runs of every kind and state, and the runs needed must be picked from it.
// GitHub answers a query with a filter such as `event`, `status` or `created` by a search, which has its own limit of 1,000 results (docs).
// GitHub also says that such searches often time out, and then report only what they found before the timeout (docs).
// With an `event`, `status` or `created` filter, GitHub now and then answered an empty list although the runs exist
// (2026-10-22, about 1 in 4 answers, measured).
// Without filters, every answer had the same runs (measured).
func readSyncWorkflowRunPage(ctx context.Context, repo *DBRepo, page int) ([]SyncWorkflowRun, error) {
	var list struct {
		Runs []SyncWorkflowRun `json:"workflow_runs"`
	}
	query := fmt.Sprintf("?per_page=%d&page=%d", syncWorkflowRunsPerPage, page)
	err := callDBRepoAPI(ctx, repo, http.MethodGet, syncWorkflowAPIPath+"/runs"+query, nil, &list)
	if hasGitHubStatus(err, http.StatusNotFound) { // If there is no workflow, its run list is 404 too
		return []SyncWorkflowRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Runs, nil
}

// Runs of the sync workflow read from the start of the run list, page by page.
// It keeps how far it has read, so reading more goes on from the next page
type syncWorkflowRunList struct {
	repo      *DBRepo           // DB repository whose workflow runs are read
	runs      []SyncWorkflowRun // Every run read so far, newest first, from the first run of the list with none left out
	pagesRead int               // Pages read so far, from page 1
	// `true` once a page had fewer runs than a full page, so the last run of the list has been read.
	// `false` only means the end hasn't been seen yet.
	// (When the last page is full, the read after it gets an empty page.)
	reachedEnd bool
}

// Reads the next page of the run list and adds its runs to `list.runs`.
// Once the end of the list has been read (`reachedEnd`), it reads nothing.
// `(list *syncWorkflowRunList)` is a pointer receiver, so the caller's list changes
func (list *syncWorkflowRunList) readNextPage(ctx context.Context) error {
	if list.reachedEnd {
		return nil
	}
	pageRuns, err := readSyncWorkflowRunPage(ctx, list.repo, list.pagesRead+1)
	if err != nil {
		return err
	}
	list.runs = append(list.runs, pageRuns...) // `append` adds the runs of the page after the runs read before
	list.pagesRead++
	list.reachedEnd = len(pageRuns) < syncWorkflowRunsPerPage
	return nil
}

// Returns the recent runs among the runs read (`isRecentWorkflowRun`), in a new slice.
// Reading more pages into `list` later doesn't change it
func (list *syncWorkflowRunList) pickRecentRuns(now time.Time) []SyncWorkflowRun {
	recentRuns := []SyncWorkflowRun{}
	for _, run := range list.runs {
		if isRecentWorkflowRun(run, now) {
			recentRuns = append(recentRuns, run)
		}
	}
	return recentRuns
}

// Reports whether `run` started on the day before `now` (UTC) or later.
// Only such runs are used to tell which run is going now, so a run left unfinished long ago isn't taken for one
func isRecentWorkflowRun(run SyncWorkflowRun, now time.Time) bool {
	year, month, day := now.UTC().AddDate(0, 0, -1).Date()
	return !run.StartedAt.Before(time.Date(year, month, day, 0, 0, 0, 0, time.UTC))
}

// Reads the run list of the sync workflow from page 1 until it has every recent run (`isRecentWorkflowRun`).
// It stops after the first page that ends with an older run, or at the end of the list.
//
// Returns:
//   - The runs read, with every recent run (`syncWorkflowRunList.pickRecentRuns`).
//     The older runs of the last page are in it too. Empty if there is no workflow
//
// NOTE: The list is newest first (measured), so every run after an older run is older too.
// There is no limit on the pages.
// Every recent run is needed to tell the run going now, and the request deadline bounds the reads.
func readRecentSyncWorkflowRuns(ctx context.Context, repo *DBRepo, now time.Time) (*syncWorkflowRunList, error) {
	list := &syncWorkflowRunList{repo: repo, runs: []SyncWorkflowRun{}}
	for !list.reachedEnd {
		if err := list.readNextPage(ctx); err != nil {
			return nil, err
		}
		if len(list.runs) > 0 && !isRecentWorkflowRun(list.runs[len(list.runs)-1], now) {
			break
		}
	}
	return list, nil
}

// Finds the finished scheduled run that started last, leaving out cancelled runs.
// It looks in the runs read first, and reads more pages into `list` only if no such run is there, up to `maxScheduledRunPages` pages in all.
// The list is newest first (measured), so the first page that has one holds the last one.
//
// Returns:
//   - The run. `nil` if the pages read have none, or if there is no workflow
//
// NOTE: While a call from the workflow is handled, the run that sent it is in the list, still in progress.
// If that run were picked as "the previous run", its result would be empty and yesterday's failure would be missed.
// So only finished runs count.
// A cancelled run isn't a failed sync, so it says nothing about the sync and the run before it counts.
// For example, `concurrency` cancels a run that waits behind another when a third one comes (docs).
// Manual runs don't count.
func findLastFinishedScheduledWorkflowRun(ctx context.Context, list *syncWorkflowRunList) (*SyncWorkflowRun, error) {
	for {
		var last *SyncWorkflowRun
		for i := range list.runs {
			run := &list.runs[i]
			if run.Event != workflowRunScheduledEvent || run.Status != workflowRunCompleted || run.Conclusion == workflowRunCancelled {
				continue
			}
			if last == nil || run.StartedAt.After(last.StartedAt) {
				last = run
			}
		}
		if last != nil || list.reachedEnd || list.pagesRead >= maxScheduledRunPages {
			return last, nil
		}
		if err := list.readNextPage(ctx); err != nil {
			return nil, err
		}
	}
}

// Finds in `recentRuns` the active run (one of any kind that hasn't finished), preferring one whose job is running.
//
// Args:
//   - `recentRuns`: Every recent run (`syncWorkflowRunList.pickRecentRuns`)
//
// Returns:
//   - The run. `nil` if none is active
func findActiveWorkflowRun(recentRuns []SyncWorkflowRun) *SyncWorkflowRun {
	var active *SyncWorkflowRun
	for i := range recentRuns {
		run := &recentRuns[i]
		if run.Status == workflowRunCompleted {
			continue
		}
		if active == nil || run.Status == workflowRunInProgress {
			active = run
		}
	}
	return active
}

// Finds in `recentRuns` the finished run of any kind that started last.
//
// Args:
//   - `recentRuns`: Every recent run (`syncWorkflowRunList.pickRecentRuns`)
//
// Returns:
//   - The run. `nil` if none finished since yesterday
func findLastFinishedWorkflowRun(recentRuns []SyncWorkflowRun) *SyncWorkflowRun {
	var last *SyncWorkflowRun
	for i := range recentRuns {
		run := &recentRuns[i]
		if run.Status != workflowRunCompleted {
			continue
		}
		if last == nil || run.StartedAt.After(last.StartedAt) {
			last = run
		}
	}
	return last
}

// Reports whether the run that sent the current call was started by hand (`workflow_dispatch`).
//
// Args:
//   - `recentRuns`: Every recent run (`syncWorkflowRunList.pickRecentRuns`)
//
// NOTE: The call doesn't say which run sent it, and the installed workflow can't be changed to say it.
// While a call from the workflow is handled, the run that sent it is in the list as `in_progress` (measured).
// The workflow's `concurrency` lets only one run go at a time (docs, measured).
// So the run that is `in_progress` is the run that sent the call.
// If the list doesn't show any run as `in_progress` yet, a manual run that hasn't finished counts.
// Then a Sync now isn't taken for a scheduled run that isn't due, and the worst case is one extra sync.
func isCallFromManualWorkflowRun(recentRuns []SyncWorkflowRun) bool {
	for _, run := range recentRuns {
		if run.Status == workflowRunInProgress {
			return run.Event == workflowRunManualEvent
		}
	}
	for _, run := range recentRuns {
		if run.Status != workflowRunCompleted && run.Event == workflowRunManualEvent {
			return true
		}
	}
	return false
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
