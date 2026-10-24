// Metadata snapshots in the archive (`db/repo-meta.json`, `RepoMeta`): each repository's info as it last was on GitHub, kept as a record.
// They are brought up to date by `SyncRepoMetaSnapshotsWithGitHub`, and written by `UpdateRepoMetaSnapshots`.
// Reading the current values from GitHub is in `github_repo_meta.go`.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"slices"
	"sync"
	"time"
)

// Most time one sync spends refreshing the metadata snapshots.
// A request for 100 repositories takes about 4.5 seconds (measured), and 6 are sent at once.
// So this is enough to check thousands at a time.
// Repositories not checked yet get checked in the next sync.
// It is capped so that a large archive can't use up the time of the whole sync
const repoMetaSyncBudget = 40 * time.Second

// One repository metadata snapshot (one line of `db/repo-meta.json`).
// It keeps the last checked values, so "what it was" remains even if the repository is deleted or made private.
// The field order is the key order in the file.
// Changing the order changes the whole file in the diff
type RepoMeta struct {
	ID               int64    `json:"id"`                // ID of the archive item (GitHub repository ID)
	Name             string   `json:"name"`              // Last checked `owner/repo`. Follows renames
	Description      string   `json:"description"`       // Description
	Language         string   `json:"language"`          // Main language. Empty string if there is none
	LanguageColor    string   `json:"language_color"`    // Display color of that language, set by GitHub (`#rrggbb`)
	Topics           []string `json:"topics"`            // Topics. An empty array if there are none (not `null`)
	Homepage         string   `json:"homepage"`          // Homepage address
	License          string   `json:"license"`           // License SPDX ID (e.g. `MIT`). Empty string if there is none
	Archived         bool     `json:"archived"`          // Whether the owner archived it (read-only)
	UpdatedAt        string   `json:"updated_at"`        // Time the snapshot content last changed (or was first made). Left as is when a check finds no change
	UnavailableSince string   `json:"unavailable_since"` // Time GitHub first answered that it can't find this repository. Empty string once it is visible again
}

// Reports whether two snapshots have the same repository info.
// The ID and the time fields (`UpdatedAt`, `UnavailableSince`) aren't compared
func isSameRepoMeta(a RepoMeta, b RepoMeta) bool {
	return a.Name == b.Name && a.Description == b.Description && a.Language == b.Language && a.LanguageColor == b.LanguageColor &&
		slices.Equal(a.Topics, b.Topics) && a.Homepage == b.Homepage && a.License == b.License && a.Archived == b.Archived
}

// Number of requests sent to GitHub at the same time when refreshing snapshots.
// One request for 100 repositories takes about 4.5 seconds (measured).
// Requests must go out in parallel to check thousands in time
const repoMetaFetchParallel = 6

// Result of one snapshot refresh
type RepoMetaSyncResult struct {
	NumCheckedRepos    int     // Number of repositories asked of GitHub this time (batches whose request failed aren't counted)
	UnavailableRepoIDs []int64 // IDs of the repositories in the "not found" state after the refresh
}

// Refreshes the metadata snapshots (`db/repo-meta.json`) of the repositories in the archive with GitHub's current values.
//   - Shuffles the repositories and asks 100 at a time.
//     A new request is sent only if it can finish within `budget` even when the answer takes the longest wait (`githubTimeout`)
//   - Updates the snapshot of each repository found.
//     If the content is the same, `UpdatedAt` stays too.
//     If nothing changed, no commit is made
//   - For a repository that GitHub answered as "not found", the snapshot content stays.
//     Only the time of the first such answer is kept (`UnavailableSince`)
//   - A batch whose request failed changes nothing.
//     If every batch sent failed with a failure that needs a person (`isTemporaryGitHubError` is false), it stops with that error
//   - A rate limit answer stops it from sending more batches.
//     The batches answered before it are still saved
//   - The result is written in one commit.
//     The subject has the number checked and the number not found.
//     The body lists disappeared, visible again and renamed repositories, and the number whose details changed
//
// Args:
//   - `budget`: Time for the requests to GitHub. The commit at the end comes after it
//
// Returns:
//   - The number checked and the IDs not found
//   - An error from reading the archive or from the commit, or the failure of one batch when every batch sent failed with a failure that needs a person.
//     When every batch sent failed with a GitHub failure that passes on its own, it isn't an error, and the next sync goes on
//
// NOTE: Because of the random order, different repositories get checked in each run, even when there are too many to check at once.
// Items have no check time, so the file doesn't change when the content is the same.
// The commit diff isn't buried under lines that change every day.
// A disappearance is recorded only when GitHub clearly answers `NOT_FOUND`.
// A temporary outage must not mark hundreds of repositories as gone.
// Repositories are never deleted automatically.
func SyncRepoMetaSnapshotsWithGitHub(ctx context.Context, repo *DBRepo, budget time.Duration) (RepoMetaSyncResult, error) {
	budgetEndsAt := time.Now().Add(budget)
	starItems, err := starItemsDataFile.readLatestFromDBRepo(ctx, repo)
	if err != nil {
		return RepoMetaSyncResult{}, err
	}
	targets := []StarItem{}
	for _, s := range starItems {
		if s.NodeID != "" { // An item without `node_id` can't be asked about with GraphQL
			targets = append(targets, s)
		}
	}
	rand.Shuffle(len(targets), func(i int, j int) { targets[i], targets[j] = targets[j], targets[i] }) // Shuffled, so each run checks different repositories even if it can't check them all in time
	batches := [][]StarItem{}
	for start := 0; start < len(targets); start += MaxRepoMetaNodeIDs {
		batches = append(batches, targets[start:min(start+MaxRepoMetaNodeIDs, len(targets))])
	}

	// Several goroutines (lightweight threads) take batches one by one and process them.
	// Shared values are read and written while holding `mu`
	var mu sync.Mutex
	next := 0
	checked := 0
	failedBatches := 0
	var failure error             // The failure of one batch. One that needs a person is kept over one that passes on its own
	var rateLimited error         // The first rate limit answer. Once it is set, no more batches are sent
	fresh := map[int64]RepoMeta{} // New snapshots of the repositories found (repository info only)
	gone := map[int64]bool{}      // Repositories that GitHub answered it can't find
	var wg sync.WaitGroup         // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < repoMetaFetchParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				// Send the next request only if it ends before `budgetEndsAt` even when it waits the longest for GitHub (`githubTimeout`)
				if rateLimited != nil || next >= len(batches) || time.Now().Add(githubTimeout).After(budgetEndsAt) {
					mu.Unlock()
					return
				}
				batch := batches[next]
				next++
				mu.Unlock()

				ids := make([]string, len(batch))
				for i, s := range batch {
					ids[i] = s.NodeID
				}
				// Only the call runs through `RunWithPanicAsError`, not the code that holds `mu`.
				// A panic becomes the error of this batch
				var nodes []json.RawMessage
				var missing []string
				err := RunWithPanicAsError(func() error {
					var err error
					nodes, missing, err = FetchRepoMetaByNodeIDs(ctx, repo.cfg, ids)
					return err
				})
				if err != nil {
					log.Printf("repo details failed (%d repos left unchanged): %v", len(batch), err)
					mu.Lock()
					failedBatches++
					if failure == nil || isTemporaryGitHubError(ctx, failure) {
						failure = err
					}
					if rateLimited == nil && errors.Is(err, errGitHubRateLimited) {
						rateLimited = err
					}
					mu.Unlock()
					continue // A batch whose request failed changes nothing. This keeps a temporary outage from being recorded as a disappearance
				}
				mu.Lock()
				checked += len(batch)
				for i, node := range nodes {
					if node == nil {
						continue
					}
					if meta, err := buildRepoMeta(node); err == nil {
						fresh[batch[i].ID] = meta
					}
				}
				for _, id := range missing {
					gone[batch[slices.Index(ids, id)].ID] = true
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if rateLimited != nil {
		unsent := 0
		for _, batch := range batches[next:] {
			unsent += len(batch)
		}
		log.Printf("repo details: stopped with %d repos left for the next sync: %v", unsent, rateLimited)
	}
	// Every batch sent failed, so nothing was checked.
	// If a person must fix the cause (for example, GitHub no longer accepts the query), the refresh and the disappearance checks would stop silently.
	// So that case is an error
	if failedBatches > 0 && checked == 0 && !isTemporaryGitHubError(ctx, failure) {
		return RepoMetaSyncResult{}, fmt.Errorf("every repository details request failed: %w", failure)
	}

	now := FormatNow()
	updated, err := UpdateRepoMetaSnapshots(ctx, repo, func(starItems []StarItem, repoMeta *[]RepoMeta) (CommitMessage, error) {
		byID := make(map[int64]RepoMeta, len(*repoMeta))
		for _, m := range *repoMeta {
			byID[m.ID] = m
		}
		// Notable changes for the commit body.
		// If called again after a conflict, count again from zero
		vanished := []string{}
		revived := []string{}
		renamed := []string{}
		changed := 0
		unavailable := 0

		// Rebuilds the snapshot list in the order of the archive items at the same commit.
		// Snapshots of repositories removed from the archive drop out here
		next := []RepoMeta{}
		for _, s := range starItems {
			old, known := byID[s.ID]
			m := old
			if snap, ok := fresh[s.ID]; ok {
				snap.ID, snap.UnavailableSince, snap.UpdatedAt = s.ID, "", old.UpdatedAt
				if !known || !isSameRepoMeta(old, snap) {
					snap.UpdatedAt = now // Only a snapshot whose content changed or that is new gets a new time
				}
				switch {
				case old.UnavailableSince != "":
					revived = append(revived, snap.Name)
				case known && old.Name != snap.Name:
					renamed = append(renamed, old.Name+" -> "+snap.Name)
				case known && !isSameRepoMeta(old, snap):
					changed++
				}
				m, known = snap, true
			} else if gone[s.ID] { // Only repositories that GitHub clearly answered with `NOT_FOUND` are recorded as gone
				if !known {
					m = RepoMeta{ID: s.ID, Name: s.Name, Topics: []string{}, UpdatedAt: now} // A repository that was gone from its first check. The only thing known is the name stored in the archive
					known = true
				}
				if m.UnavailableSince == "" {
					m.UnavailableSince = now
					vanished = append(vanished, m.Name)
				}
			}
			if known { // A repository not checked this time and without an earlier snapshot isn't added
				if m.UnavailableSince != "" {
					unavailable++
				}
				next = append(next, m)
			}
		}

		details := ""
		if changed > 0 {
			details = "Details changed: " + FormatCount(changed, "repo")
		}
		commitMessage := NewCommitMessage(
			"sync(repo-meta)", "refresh "+FormatCount(checked, "repo")+fmt.Sprintf(", %d unavailable", unavailable),
			formatCommitList("Unavailable", vanished), formatCommitList("Available again", revived), formatCommitList("Renamed", renamed), details,
		)
		*repoMeta = next
		return commitMessage, nil
	})
	if err != nil {
		return RepoMetaSyncResult{}, err
	}
	result := RepoMetaSyncResult{NumCheckedRepos: checked}
	for _, m := range updated {
		if m.UnavailableSince != "" {
			result.UnavailableRepoIDs = append(result.UnavailableRepoIDs, m.ID)
		}
	}
	return result, nil
}

// Reads the archive items and the metadata snapshots from one commit, changes the snapshots with `change`, and writes them in one commit if they changed.
// The items are only read.
// Changes to `starItems` are not written.
// It works on top of `UpdateDBRepoFiles`.
//
// Args:
//   - `change`: Changes `repoMeta` in place, or replaces it, and returns a commit message.
//     If it returns an error, nothing is written.
//     If another commit gets in, it is called again with the files read at the new commit
//
// Returns:
//   - The snapshots as committed. If they didn't change, nothing is committed and they are returned as is
func UpdateRepoMetaSnapshots(ctx context.Context, repo *DBRepo, change func(starItems []StarItem, repoMeta *[]RepoMeta) (CommitMessage, error)) ([]RepoMeta, error) {
	var committed []RepoMeta
	err := UpdateDBRepoFiles(ctx, repo, []string{starItemsDataFile.path, repoMetaDataFile.path}, func(filesAtCommit *DBRepoFilesAtCommit) (FileChangesCommit, error) {
		starItems, err := starItemsDataFile.decodeForReading(filesAtCommit) // Only read, so changes to it aren't written
		if err != nil {
			return FileChangesCommit{}, err
		}
		var repoMeta []RepoMeta
		trackedRepoMeta, err := repoMetaDataFile.decodeForWriting(filesAtCommit, &repoMeta)
		if err != nil {
			return FileChangesCommit{}, err
		}
		message, err := change(starItems, &repoMeta)
		if err != nil {
			return FileChangesCommit{}, err
		}
		changes := FileChangesCommit{FilesToWrite: map[string][]byte{}, Message: message}
		if err := trackedRepoMeta.addIfChanged(&changes); err != nil {
			return FileChangesCommit{}, err
		}
		committed = repoMeta // If called again, this becomes the new result
		return changes, nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
}
