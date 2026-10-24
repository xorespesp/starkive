// Reading from GitHub: my Stars (`FetchStarredPage`, `FetchAllStarredRepos`) and my GitHub star lists (`FetchGitHubStarListsByRepo`).
// Putting Stars into the archive is in `archive_stars.go`.
package core

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
)

// One item of the GitHub API that lists Stars (`Accept: application/vnd.github.star+json`).
// Only the fields in use are read
type StarredRepo struct {
	StarredAt string `json:"starred_at"` // Time the Star was given (the string from GitHub as it is). Becomes `StarredAt` in the archive
	Repo      struct {
		ID              int64  `json:"id"`               // Repository ID. Becomes the ID of the archive item (`StarItem.ID`)
		NodeID          string `json:"node_id"`          // ID for GraphQL queries
		FullName        string `json:"full_name"`        // `owner/repo`
		StargazersCount int    `json:"stargazers_count"` // Number of Stars. Only shown on the Import screen, not stored in the archive
	} `json:"repo"`
}

// Number of items per page of Stars.
// The largest value GitHub allows (docs)
const StarsPerPage = 100

// Regular expression that takes the last page number from the `Link` response header.
// Header shape: `<...&page=30>; rel="last"`
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
//   - An error that wraps `ErrGitHubRateLimited` if GitHub answers with the rate limit
//
// NOTE: Each page can be read on its own.
// So even thousands of Stars can be split into several requests and read at the same time.
// In the dashboard, one request becomes one GitHub call, so it stays within the function time limit.
func FetchStarredPage(ctx context.Context, cfg *EnvConfig, page int) ([]StarredRepo, int, error) {
	res, err := CallGitHubAPI(ctx, cfg, fmt.Sprintf("/user/starred?per_page=%d&page=%d", StarsPerPage, page),
		GitHubAPIRequest{Accept: "application/vnd.github.star+json"})
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if err := CheckGitHubResponseWithRateLimit(res); err != nil {
		return nil, 0, err
	}
	starredRepos := []StarredRepo{}
	if err := DecodeGitHubResponse(res, &starredRepos); err != nil {
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
// If any page fails, it reads no more pages and returns that error.
//
// Returns:
//   - The Stars, in GitHub's order (most recently starred first). Each repository appears only once
//
// NOTE: If Stars are added or removed while reading, the page boundaries shift.
// Then the same repository can come on two pages, so duplicates are removed by repository ID.
// The result needs every page, so after one page fails, reading the rest would only use up the rate limit.
func FetchAllStarredRepos(ctx context.Context, cfg *EnvConfig) ([]StarredRepo, error) {
	first, lastPage, err := FetchStarredPage(ctx, cfg, 1)
	if err != nil {
		return nil, err
	}
	pages := make([][]StarredRepo, lastPage)
	pages[0] = first
	var mu sync.Mutex // Goroutines (lightweight threads) share `next` and `failure`, so they are locked
	next := 2
	var failure error     // The error of the first page that failed. Once it is set, no more pages are read
	var wg sync.WaitGroup // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < starredPagesParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if failure != nil || next > lastPage {
					mu.Unlock()
					return
				}
				page := next
				next++
				mu.Unlock()
				// Only the call runs through `RunWithPanicAsError`, not the code that holds `mu`.
				// A panic becomes the error of this page
				err := RunWithPanicAsError(func() error {
					starredRepos, _, err := FetchStarredPage(ctx, cfg, page)
					pages[page-1] = starredRepos // Each page has its own slot, so writing without a lock doesn't overlap
					return err
				})
				if err != nil {
					mu.Lock()
					if failure == nil {
						failure = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if failure != nil {
		return nil, failure
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
func FetchGitHubStarListsByRepo(ctx context.Context, cfg *EnvConfig) (map[int64][]string, error) {
	// Reads my GitHub star lists and the first 100 repositories in each.
	// If there are more than 100 star lists, reading continues with `$after`
	const listsQuery = `query($after: String) {
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
	// Reads the repositories of one GitHub star list, continuing after the first 100
	const itemsQuery = `query($id: ID!, $after: String) {
		node(id: $id) {
			... on UserList {
				items(first: 100, after: $after) { pageInfo { hasNextPage endCursor } nodes { ... on Repository { databaseId } } }
			}
		}
	}`
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
		if err := callGraphQL(ctx, cfg, listsQuery, map[string]any{"after": after}, &data); err != nil {
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
				if err := callGraphQL(ctx, cfg, itemsQuery, map[string]any{"id": githubStarList.ID, "after": page.PageInfo.EndCursor}, &more); err != nil {
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
