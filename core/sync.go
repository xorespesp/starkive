// Sync that brings the archive in line with GitHub (`SyncArchiveWithGitHub`).
// It adds new Stars (`archive_star_items.go`), then refreshes the metadata snapshots (`archive_repo_meta.go`) and the README snapshots (`archive_readmes.go`).
// When to sync (the schedule decision, turning it on and off) is in `sync_schedule.go`.
package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// Counts of one sync, written to the server log
type ArchiveSyncResult struct {
	NumStarredRepos     int // Number of repositories starred on GitHub
	NumAddedRepos       int // Number newly archived this time
	NumMetaCheckedRepos int // Number of repositories whose metadata snapshot was checked
	NumUnavailableRepos int // Number of repositories that can't be found on GitHub
	NumCheckedReadmes   int // Number of READMEs fetched and compared with the saved ones this time
}

// Brings the archive in line with GitHub.
//  1. Adds my GitHub Stars that aren't in the archive (`ArchiveStarredRepos`)
//  2. Refreshes the metadata snapshots within `repoMetaSyncBudget`, or the time left before `longStepsEndBy` if that is shorter (`SyncRepoMetaSnapshotsWithGitHub`)
//  3. Refreshes the README snapshots with the time left before `longStepsEndBy` (`SyncReadmeSnapshotsWithGitHub`).
//     Repositories that the metadata step found missing aren't asked.
//     If that step failed, none are skipped.
//     With `refreshReadmesEarly`, it also uses the time left to refresh the saved READMEs whose turn to be refreshed hasn't come yet
//
// Args:
//   - `ctx`: Deadline of the whole sync. Every request, commits included, ends by it
//   - `longStepsEndBy`: Time after which the metadata and README steps fetch nothing more from GitHub, earlier than the deadline of `ctx`.
//     A commit or read they start before it can end after it, by the deadline of `ctx`.
//     They leave what is left for the next sync.
//     Adding new Stars doesn't check it, but its requests end by the deadline of `ctx`
//   - `refreshReadmesEarly`: Whether the README step also uses the time left to refresh the saved READMEs whose turn to be refreshed hasn't come yet
//
// Returns:
//   - The counts (`ArchiveSyncResult`). Even if a step failed, the counts of the steps that succeeded are filled in
//   - The errors of the failed steps joined together, each with its step name ("archive new stars", "refresh repo meta", "refresh readmes").
//     The README step leaves out GitHub failures that pass on their own (`SyncReadmeSnapshotsWithGitHub`)
//
// NOTE: The steps are independent, so even if one fails the others run to the end.
// The exception is a GraphQL rate limit answer: after it, the later steps run but send no GraphQL queries (`gitHubRateLimitMark`).
// Adding comes first, so new items usually get their metadata and README snapshots in the same run.
// If the branch just written briefly shows the old commit (measured), or time runs short, they get them in the next run.
// Each run compares all Stars with the archive.
// So overlapping runs or a skipped day give the same result.
// GitHub Stars are never changed.
func SyncArchiveWithGitHub(ctx context.Context, repo *DBRepo, longStepsEndBy time.Time, refreshReadmesEarly bool) (ArchiveSyncResult, error) {
	result := ArchiveSyncResult{}
	var errs []error
	syncStart := time.Now() // For the total time in the log
	stepStart := syncStart  // Start of the current step, for the step times in the log

	starredRepos, err := FetchAllStarredRepos(ctx, repo.cfg)
	if err == nil {
		result.NumStarredRepos = len(starredRepos)
		result.NumAddedRepos, err = ArchiveStarredRepos(ctx, repo, starredRepos, []int64{}, "sync(star-items)")
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("archive new stars: %w", err)) // Wrapped with `%w`, so `errors.As` still finds the inner error (`HTTPError`, `GitHubAPIError`)
	}
	starsTime := time.Since(stepStart)
	stepStart = time.Now()

	meta, err := SyncRepoMetaSnapshotsWithGitHub(ctx, repo, min(repoMetaSyncBudget, time.Until(longStepsEndBy))) // `min`: built into Go since 1.21
	if err == nil {
		result.NumMetaCheckedRepos, result.NumUnavailableRepos = meta.NumCheckedRepos, len(meta.UnavailableRepoIDs)
	} else {
		errs = append(errs, fmt.Errorf("refresh repo meta: %w", err))
	}
	metaTime := time.Since(stepStart)
	stepStart = time.Now()

	result.NumCheckedReadmes, err = SyncReadmeSnapshotsWithGitHub(ctx, repo, longStepsEndBy, meta.UnavailableRepoIDs, time.Now(), refreshReadmesEarly)
	if err != nil {
		errs = append(errs, fmt.Errorf("refresh readmes: %w", err))
	}

	readmesTime := time.Since(stepStart)

	round := func(d time.Duration) time.Duration { return d.Round(100 * time.Millisecond) } // Printed like `1.2s`
	log.Printf("starred: %d, new: %d, metadata checked: %d, unavailable: %d, readmes checked: %d, errors: %v, took: total %v (stars %v, meta %v, readmes %v)",
		result.NumStarredRepos, result.NumAddedRepos, result.NumMetaCheckedRepos, result.NumUnavailableRepos, result.NumCheckedReadmes, errs,
		round(time.Since(syncStart)), round(starsTime), round(metaTime), round(readmesTime))
	return result, errors.Join(errs...)
}
