// Sync that brings the archive in line with GitHub.
// It reads my GitHub Stars and GitHub star lists, puts Stars into the archive, and ties them together in `Sync`.
// When to sync (the schedule decision, turning it on and off) is in `sync_schedule.go`.
// Refreshing the metadata snapshots is in `meta.go`.
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"
)

// One item of the GitHub API that lists Stars (`Accept: application/vnd.github.star+json`). Only the fields in use are read
type StarredRepo struct {
	StarredAt string `json:"starred_at"` // Time the Star was given (the string from GitHub as it is). Becomes `StarredAt` in the archive
	Repo      struct {
		ID              int64  `json:"id"`               // Repository ID. Becomes the ID of the archive item (`StarItem.ID`)
		NodeID          string `json:"node_id"`          // ID for GraphQL queries
		FullName        string `json:"full_name"`        // `owner/repo`
		StargazersCount int    `json:"stargazers_count"` // Number of Stars. Only shown on the Import screen, not stored in the archive
	} `json:"repo"`
}

// Number of items per page of Stars. The largest value GitHub allows (docs)
const StarsPerPage = 100

// Regular expression that takes the last page number from the `Link` response header. Header shape: `<...&page=30>; rel="last"`
var lastPageLink = regexp.MustCompile(`[?&]page=(\d+)>; rel="last"`)

// Reads one page of my GitHub Stars (at most `StarsPerPage` items).
// It uses the token's account permission Starring (read).
//
// Args:
//   - `page`: Page number, counted from 1
//
// Returns:
//   - Stars on that page (most recently starred first)
//   - The last page number. Same as `page` if there is only one page
//
// NOTE: Each page can be read on its own.
// So even thousands of Stars can be split into several requests and read at the same time.
// In the dashboard, one request becomes one GitHub call, so it stays within the function time limit.
func FetchStarredPage(cfg *Config, page int) ([]StarredRepo, int, error) {
	res, err := CallGitHubAPI(cfg, fmt.Sprintf("/user/starred?per_page=%d&page=%d", StarsPerPage, page),
		GitHubAPIRequest{Accept: "application/vnd.github.star+json"})
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return nil, 0, err
	}
	starredRepos := []StarredRepo{}
	if err := json.NewDecoder(res.Body).Decode(&starredRepos); err != nil {
		return nil, 0, err
	}
	lastPage := page // If the `Link` header has no `rel="last"`, this is the last page
	if m := lastPageLink.FindStringSubmatch(res.Header.Get("Link")); m != nil {
		lastPage, _ = strconv.Atoi(m[1])
	}
	return starredRepos, lastPage, nil
}

// Number of pages read at the same time when reading all Stars
const starredPagesParallel = 6

// Reads all my GitHub Stars.
// The first page tells the last page number.
// Then the rest is read `starredPagesParallel` pages at a time.
// If any page fails, it returns an error.
//
// Returns:
//   - The Stars, in GitHub's order (most recently starred first). Each repository appears only once
//
// NOTE: If Stars are added or removed while reading, the page boundaries shift.
// Then the same repository can come on two pages, so duplicates are removed by repository ID.
func FetchAllStarredRepos(cfg *Config) ([]StarredRepo, error) {
	first, lastPage, err := FetchStarredPage(cfg, 1)
	if err != nil {
		return nil, err
	}
	pages := make([][]StarredRepo, lastPage)
	pages[0] = first
	errs := make([]error, lastPage)
	var mu sync.Mutex // Goroutines (lightweight threads) share `next`, so it is locked
	next := 2
	var wg sync.WaitGroup // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < starredPagesParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				page := next
				next++
				mu.Unlock()
				if page > lastPage {
					return
				}
				pages[page-1], _, errs[page-1] = FetchStarredPage(cfg, page) // Each page has its own slot, so writing without a lock doesn't overlap
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	seen := map[int64]bool{}
	starredRepos := []StarredRepo{}
	for _, page := range pages {
		for _, s := range page {
			if !seen[s.Repo.ID] { // For a repository that came twice because the page boundaries shifted, keep only the first
				seen[s.Repo.ID] = true
				starredRepos = append(starredRepos, s)
			}
		}
	}
	return starredRepos, nil
}

// GraphQL query that reads my GitHub star lists and the first 100 repositories in each.
// If there are more than 100 star lists, reading continues with `$after`
const githubStarListsQuery = `query($after: String) {
  viewer {
    lists(first: 100, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes {
        id
        name
        items(first: 100) { pageInfo { hasNextPage endCursor } nodes { ... on Repository { databaseId } } }
      }
    }
  }
}`

// GraphQL query that reads the repositories of one GitHub star list, continuing after the first 100
const githubStarListItemsQuery = `query($id: ID!, $after: String) {
  node(id: $id) {
    ... on UserList {
      items(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ... on Repository { databaseId } } }
    }
  }
}`

// One batch of repositories in a GitHub star list (GraphQL `items`), and whether there is a next batch
type githubStarListItems struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"` // Whether more batches follow
		EndCursor   string `json:"endCursor"`   // Value passed as `$after` to read the next batch
	} `json:"pageInfo"`
	Nodes []struct {
		DatabaseID int64 `json:"databaseId"` // Repository ID (same value as `StarItem.ID`). 0 for an item that isn't a repository
	} `json:"nodes"`
}

// Reads my GitHub star list membership per repository.
//
// Returns:
//   - Repository ID -> names of the GitHub star lists that contain it. A repository in no star list has no key
//
// NOTE: GitHub gives "star list -> repositories".
// When adding to the archive, the question is "which star lists is this repository in?".
// So the result is turned around.
// Star lists and the repositories in them both come 100 at a time.
// So both are read until there is no next batch.
func FetchGitHubStarListsByRepo(cfg *Config) (map[int64][]string, error) {
	byRepo := map[int64][]string{}
	addItems := func(name string, items githubStarListItems) {
		for _, n := range items.Nodes {
			if n.DatabaseID != 0 {
				byRepo[n.DatabaseID] = append(byRepo[n.DatabaseID], name) // Turns "star list -> repository" around into "repository -> star list"
			}
		}
	}

	var after any // `nil` (null in GraphQL) for the first request, then the `endCursor` of the previous answer
	for {
		var data struct {
			Viewer struct {
				Lists struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID    string              `json:"id"`
						Name  string              `json:"name"`
						Items githubStarListItems `json:"items"`
					} `json:"nodes"`
				} `json:"lists"`
			} `json:"viewer"`
		}
		if err := callGraphQL(cfg, githubStarListsQuery, map[string]any{"after": after}, &data); err != nil {
			return nil, err
		}
		for _, githubStarList := range data.Viewer.Lists.Nodes {
			addItems(githubStarList.Name, githubStarList.Items)
			for page := githubStarList.Items; page.PageInfo.HasNextPage; { // For a star list with more than 100 repositories, read the rest
				var more struct {
					Node struct {
						Items githubStarListItems `json:"items"`
					} `json:"node"`
				}
				if err := callGraphQL(cfg, githubStarListItemsQuery, map[string]any{"id": githubStarList.ID, "after": page.PageInfo.EndCursor}, &more); err != nil {
					return nil, err
				}
				page = more.Node.Items
				addItems(githubStarList.Name, page)
			}
		}
		if !data.Viewer.Lists.PageInfo.HasNextPage {
			return byRepo, nil
		}
		after = data.Viewer.Lists.PageInfo.EndCursor // If there are more than 100 star lists, read the next batch
	}
}

// Puts GitHub Stars at the front of the archive as new items.
// Every path that puts Stars into the archive (sync, Import) goes through this function.
//   - Skips repositories already in the archive (by ID), and repositories repeated within `starredRepos`. Existing items are left as they are
//   - Puts new items in the star lists of `starListIDs` and in the star lists they belonged to on GitHub.
//     If the archive has no star list with the same name, one is made
//   - New star lists and new items are written together in one commit.
//     The commit subject is `<commitPrefix>: archive N stars`. The body lists the added repositories and the names of new star lists
//
// Args:
//   - `starredRepos`: Candidates to add, added in order (in GitHub's order, the most recently starred come first)
//   - `starListIDs`: Star list IDs that all new items go into (chosen by a person). An empty list if none.
//     A 400 `HTTPError` if an ID isn't in the archive
//   - `commitPrefix`: First part of the commit subject, `<who>(star-items)` (e.g. `sync(star-items)`)
//
// Returns:
//   - Number of items actually added
//
// NOTE: The duplicate check and the star list check are both done inside the `change` of `UpdateArchive`.
// That is, they use the archive right before the commit.
// So sending the same request again gives the same result.
// Star lists are made only when there are items to add.
// No item points to a star list that doesn't exist.
func ArchiveStarredRepos(cfg *Config, starredRepos []StarredRepo, starListIDs []int64, commitPrefix string) (int, error) {
	// GitHub star lists are read only when there are new repositories to add (most daily syncs have none).
	// They aren't read again when `change` is called again after a conflict.
	// They are input from GitHub, not the archive, so another commit doesn't make them stale
	var githubStarLists map[int64][]string
	added := 0
	_, err := UpdateArchive(cfg, func(a *Archive) (CommitMessage, error) {
		added = 0 // If called again after a conflict, count again from zero
		// Both the star list check and the duplicate check below use the archive right before the commit (`a`)
		requested, err := a.CheckStarListIDs(starListIDs)
		if err != nil {
			return CommitMessage{}, err
		}
		seen := make(map[int64]bool, len(a.StarItems)+len(starredRepos))
		for _, s := range a.StarItems {
			seen[s.ID] = true
		}
		if !slices.ContainsFunc(starredRepos, func(s StarredRepo) bool { return !seen[s.Repo.ID] }) {
			return CommitMessage{}, nil // Nothing to add. No file changed, so no commit is made
		}
		if githubStarLists == nil {
			if githubStarLists, err = FetchGitHubStarListsByRepo(cfg); err != nil {
				return CommitMessage{}, err
			}
		}
		fresh := []StarItem{}
		createdStarLists := []string{}
		for _, s := range starredRepos {
			if seen[s.Repo.ID] {
				continue
			}
			seen[s.Repo.ID] = true // Even if the same repository appears again in `starredRepos`, add it only once
			idByName, created := a.getOrCreateStarListIDs(githubStarLists[s.Repo.ID])
			createdStarLists = append(createdStarLists, created...)
			ids := append([]int64{}, requested...) // Appends to a copy. Appending to `requested` directly could overwrite its array when it has spare capacity
			for _, id := range idByName {
				ids = append(ids, id)
			}
			fresh = append(fresh, StarItem{
				ID: s.Repo.ID, NodeID: s.Repo.NodeID, Name: s.Repo.FullName, StarListIDs: a.CleanStarListIDs(ids),
				StarredAt: s.StarredAt,
			})
		}
		added = len(fresh)
		names := make([]string, len(fresh))
		for i, s := range fresh {
			names[i] = s.Name
		}
		a.StarItems = append(fresh, a.StarItems...)
		return NewCommitMessage(commitPrefix, "archive "+FormatCount(added, "star"),
			formatCommitList("", names), formatCommitList("New star lists", createdStarLists)), nil
	})
	return added, err
}

// Result of one sync. It is the answer to Sync now in the dashboard, and it is also written to the server log
type SyncResult struct {
	NumStarredRepos     int `json:"starred"`         // Number of repositories starred on GitHub
	NumAddedRepos       int `json:"added"`           // Number newly archived this time
	NumMetaCheckedRepos int `json:"meta_checked"`    // Number of repositories whose metadata snapshot was checked
	NumUnavailableRepos int `json:"unavailable"`     // Number of repositories that can't be found on GitHub
	NumCheckedReadmes   int `json:"readmes_checked"` // Number of READMEs fetched and compared with the saved ones this time
}

// Brings the archive in line with GitHub.
//  1. Adds my GitHub Stars that aren't in the archive (`ArchiveStarredRepos`)
//  2. Refreshes the metadata snapshots within `metaRefreshBudget`, or the time left before `deadline` if that is shorter (`RefreshMeta`)
//  3. Refreshes the README snapshots with the time left before `deadline` (`SyncReadmeSnapshotsWithGitHub`).
//     Repositories that the metadata step found missing aren't asked. If that step failed, none are skipped
//
// Args:
//   - `deadline`: Time after which the metadata and README steps start no new request.
//     They leave what is left for the next sync.
//     Adding new Stars doesn't check it
//
// Returns:
//   - The counts (`SyncResult`). Even if a step failed, the counts of the steps that succeeded are filled in
//   - The errors of the failed steps joined together, each with its step name ("archive new stars", "refresh repo meta", "refresh readmes")
//
// NOTE: The steps are independent, so even if one fails the others run to the end.
// Adding comes first, so new items usually get a snapshot in the same run.
// If the branch just written briefly shows the old commit, or the budget runs short, they get it in the next run.
// Each run compares all Stars with the archive.
// So overlapping runs or a skipped day give the same result.
// GitHub Stars are never changed.
func Sync(cfg *Config, deadline time.Time) (SyncResult, error) {
	result := SyncResult{}
	var errs []error

	starredRepos, err := FetchAllStarredRepos(cfg)
	if err == nil {
		result.NumStarredRepos = len(starredRepos)
		result.NumAddedRepos, err = ArchiveStarredRepos(cfg, starredRepos, []int64{}, "sync(star-items)")
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("archive new stars: %w", err)) // Wrapped with `%w`, so `errors.As` still finds the inner error (`HTTPError`, `GitHubAPIError`)
	}

	meta, err := RefreshMeta(cfg, min(metaRefreshBudget, time.Until(deadline))) // `min`: built into Go since 1.21
	if err == nil {
		result.NumMetaCheckedRepos, result.NumUnavailableRepos = meta.NumCheckedRepos, len(meta.UnavailableRepoIDs)
	} else {
		errs = append(errs, fmt.Errorf("refresh repo meta: %w", err))
	}

	result.NumCheckedReadmes, err = SyncReadmeSnapshotsWithGitHub(cfg, deadline, meta.UnavailableRepoIDs, time.Now())
	if err != nil {
		errs = append(errs, fmt.Errorf("refresh readmes: %w", err))
	}

	log.Printf("starred: %d, new: %d, metadata checked: %d, unavailable: %d, readmes checked: %d, errors: %v",
		result.NumStarredRepos, result.NumAddedRepos, result.NumMetaCheckedRepos, result.NumUnavailableRepos, result.NumCheckedReadmes, errs)
	return result, errors.Join(errs...)
}
