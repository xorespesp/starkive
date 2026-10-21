// Repository metadata.
// It asks GitHub for the current info of repositories (`FetchRepoMeta`),
// and refreshes the snapshots that keep it as a record (`db/repo-meta.json`, `RefreshMeta`).
package core

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Most time one sync spends refreshing the metadata snapshots.
// A request for 100 repositories takes about 4.5 seconds (measured), and 6 are sent at once.
// So this is enough to check thousands at a time. Repositories not checked yet get checked in the next sync.
// It is capped so that a large archive can't use up the time of the whole sync
const metaRefreshBudget = 40 * time.Second

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

// GitHub GraphQL query that asks for the info of many repositories at once by `node_id`.
// Star count, fork count and last push time are read only for display in the dashboard
const repoMetaQuery = `query($ids: [ID!]!) {
  nodes(ids: $ids) {
    ... on Repository {
      id
      nameWithOwner
      url
      description
      primaryLanguage { name color }
      repositoryTopics(first: 20) { nodes { topic { name } } }
      homepageUrl
      licenseInfo { spdxId }
      stargazerCount
      forkCount
      pushedAt
      isArchived
    }
  }
}`

// Largest number of IDs that one GraphQL `nodes(ids:)` call can ask for (docs)
const MaxRepoMetaIDs = 100

// Asks GitHub for the current info of many repositories at once by `node_id`.
//
// Args:
//   - `ids`: `node_id`s of the repositories to ask about. At most `MaxRepoMetaIDs`
//
// Returns:
//   - Repository info in the same order and length as `ids` (raw JSON from GraphQL). `nil` where nothing was found
//   - IDs that GitHub clearly answered as "not found" (`NOT_FOUND`), such as deleted or made private
//   - An error if the request itself failed. Then no repository may be judged as gone
//
// NOTE: If some IDs can't be accessed, GraphQL doesn't fail the whole answer.
// It puts `null` in their place, and writes the reason and position (`path: ["nodes", index]`) in `errors` (docs).
// It doesn't use `callGraphQL`, which counts any `errors` as a failure, and reads the answer itself.
// So "request failed" and "this repository doesn't exist" can be told apart.
// A temporary outage isn't recorded as a disappearance.
func FetchRepoMeta(cfg *Config, ids []string) ([]json.RawMessage, []string, error) {
	query := map[string]any{"query": repoMetaQuery, "variables": map[string]any{"ids": ids}}
	res, err := CallGitHubAPI(cfg, "/graphql", GitHubAPIRequest{Method: http.MethodPost, Body: query})
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	if err := CheckGitHubAPI(res); err != nil {
		return nil, nil, err
	}

	var result struct {
		Data struct {
			Nodes []json.RawMessage `json:"nodes"` // Each repository is kept raw, not decoded (it is passed to the dashboard as it is)
		} `json:"data"`
		Errors []struct {
			Type string `json:"type"`
			Path []any  `json:"path"` // Mixes strings and numbers like `["nodes", 1]`, so it is read as `any`. JSON numbers become `float64`
		} `json:"errors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return nil, nil, err
	}
	if len(result.Data.Nodes) != len(ids) { // If positions don't line up, we can't tell which result belongs to which repository
		return nil, nil, Errorf(http.StatusBadGateway, "GitHub GraphQL returned %d nodes for %d ids", len(result.Data.Nodes), len(ids))
	}

	nodes := make([]json.RawMessage, len(ids))
	for i, node := range result.Data.Nodes {
		if string(node) != "null" && string(node) != "{}" { // `{}` is an ID of another kind, not a repository
			nodes[i] = node
		}
	}
	missing := []string{}
	for _, e := range result.Errors { // Only positions answered with `NOT_FOUND` are "not found". Other errors aren't treated as a disappearance
		if e.Type != "NOT_FOUND" || len(e.Path) != 2 {
			continue
		}
		if i, ok := e.Path[1].(float64); ok && int(i) >= 0 && int(i) < len(ids) && nodes[int(i)] == nil { // `.(float64)`: takes the value in an `any` out as its original type
			missing = append(missing, ids[int(i)])
		}
	}
	return nodes, missing, nil
}

// Reports whether two snapshots have the same repository info. The ID and the time fields (`UpdatedAt`, `UnavailableSince`) aren't compared
func isSameSnapshot(a RepoMeta, b RepoMeta) bool {
	return a.Name == b.Name && a.Description == b.Description && a.Language == b.Language && a.LanguageColor == b.LanguageColor &&
		slices.Equal(a.Topics, b.Topics) && a.Homepage == b.Homepage && a.License == b.License && a.Archived == b.Archived
}

// Copies raw repository info from GraphQL into the repository info fields of a snapshot.
//
// Args:
//   - `node`: Raw JSON of one repository returned by `FetchRepoMeta`
//
// Returns:
//   - A snapshot with only the repository info filled in. The caller fills in the ID and the time fields
func makeSnapshot(node json.RawMessage) (RepoMeta, error) {
	var repo struct {
		NameWithOwner   string `json:"nameWithOwner"`
		Description     string `json:"description"`
		PrimaryLanguage *struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		} `json:"primaryLanguage"` // Pointer: with no language the JSON is `null`, so it becomes `nil`
		RepositoryTopics struct {
			Nodes []struct {
				Topic struct {
					Name string `json:"name"`
				} `json:"topic"`
			} `json:"nodes"`
		} `json:"repositoryTopics"`
		HomepageURL string `json:"homepageUrl"`
		LicenseInfo *struct {
			SpdxID string `json:"spdxId"`
		} `json:"licenseInfo"` // Pointer: `nil` if there is no license
		IsArchived bool `json:"isArchived"`
	}
	if err := json.Unmarshal(node, &repo); err != nil {
		return RepoMeta{}, err
	}
	meta := RepoMeta{
		Name: repo.NameWithOwner, Description: repo.Description, Topics: []string{},
		Homepage: repo.HomepageURL, Archived: repo.IsArchived,
	}
	if repo.PrimaryLanguage != nil {
		meta.Language = repo.PrimaryLanguage.Name
		meta.LanguageColor = repo.PrimaryLanguage.Color
	}
	for _, n := range repo.RepositoryTopics.Nodes {
		meta.Topics = append(meta.Topics, n.Topic.Name)
	}
	if repo.LicenseInfo != nil {
		meta.License = repo.LicenseInfo.SpdxID
	}
	return meta, nil
}

// Number of requests sent to GitHub at the same time when refreshing snapshots.
// One request for 100 repositories takes about 4.5 seconds (measured).
// Requests must go out in parallel to check thousands in time
const metaRefreshParallel = 6

// Result of one snapshot refresh
type MetaRefreshResult struct {
	NumCheckedRepos    int     // Number of repositories asked of GitHub this time (batches whose request failed aren't counted)
	UnavailableRepoIDs []int64 // IDs of the repositories in the "not found" state after the refresh
}

// Refreshes the metadata snapshots (`db/repo-meta.json`) of the repositories in the archive with GitHub's current values.
//   - Shuffles the repositories and asks 100 at a time.
//     A new request is sent only if it can finish within `budget` even when the answer takes the longest wait (`githubTimeout`)
//   - Updates the snapshot of each repository found. If the content is the same, `UpdatedAt` stays too.
//     If nothing changed, no commit is made
//   - For a repository that GitHub answered as "not found", the snapshot content stays.
//     Only the time of the first such answer is kept (`UnavailableSince`)
//   - A batch whose request failed changes nothing
//   - The result is written in one commit.
//     The subject has the number checked and the number not found.
//     The body lists disappeared, visible again and renamed repositories, and the number whose details changed
//
// Args:
//   - `budget`: Time available. It keeps the work within the time limit of the sync
//
// Returns:
//   - The number checked and the IDs not found
//
// NOTE: Because of the random order, different repositories get checked in each run, even when there are too many to check at once.
// Items have no check time, so the file doesn't change when the content is the same.
// The commit diff isn't buried under lines that change every day.
// A disappearance is recorded only when GitHub clearly answers `NOT_FOUND`.
// A temporary outage must not mark hundreds of repositories as gone.
// Repositories are never deleted automatically.
func RefreshMeta(cfg *Config, budget time.Duration) (MetaRefreshResult, error) {
	deadline := time.Now().Add(budget)
	starItems, err := readItems[StarItem](cfg, StarItemsDBPath)
	if err != nil {
		return MetaRefreshResult{}, err
	}
	targets := []StarItem{}
	for _, s := range starItems {
		if s.NodeID != "" { // An item without `node_id` can't be asked about with GraphQL
			targets = append(targets, s)
		}
	}
	rand.Shuffle(len(targets), func(i int, j int) { targets[i], targets[j] = targets[j], targets[i] }) // Shuffled, so each run checks different repositories even if it can't check them all in time
	batches := [][]StarItem{}
	for start := 0; start < len(targets); start += MaxRepoMetaIDs {
		batches = append(batches, targets[start:min(start+MaxRepoMetaIDs, len(targets))])
	}

	// Several goroutines (lightweight threads) take batches one by one and process them. Shared values are read and written while holding `mu`
	var mu sync.Mutex
	next := 0
	checked := 0
	fresh := map[int64]RepoMeta{} // New snapshots of the repositories found (repository info only)
	gone := map[int64]bool{}      // Repositories that GitHub answered it can't find
	var wg sync.WaitGroup         // Counter that waits until all goroutines end (like `std::thread::join`)
	for w := 0; w < metaRefreshParallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				// Send the next request only if it ends before `deadline` even when it waits the longest for GitHub (`githubTimeout`)
				if next >= len(batches) || time.Now().Add(githubTimeout).After(deadline) {
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
				nodes, missing, err := FetchRepoMeta(cfg, ids)
				if err != nil {
					log.Printf("repo details failed (%d repos left unchanged): %v", len(batch), err)
					continue // A batch whose request failed changes nothing. This keeps a temporary outage from being recorded as a disappearance
				}
				mu.Lock()
				checked += len(batch)
				for i, node := range nodes {
					if node == nil {
						continue
					}
					if meta, err := makeSnapshot(node); err == nil {
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

	now := FormatNow()
	updated, err := UpdateArchive(cfg, func(a *Archive) (CommitMessage, error) {
		byID := make(map[int64]RepoMeta, len(a.RepoMeta))
		for _, m := range a.RepoMeta {
			byID[m.ID] = m
		}
		// Notable changes for the commit body. If called again after a conflict, count again from zero
		vanished := []string{}
		revived := []string{}
		renamed := []string{}
		changed := 0
		unavailable := 0

		// Rebuilds the snapshot list in the order of the archive items at the same commit.
		// Snapshots of repositories removed from the archive drop out here
		next := []RepoMeta{}
		for _, s := range a.StarItems {
			old, known := byID[s.ID]
			m := old
			if snap, ok := fresh[s.ID]; ok {
				snap.ID, snap.UnavailableSince, snap.UpdatedAt = s.ID, "", old.UpdatedAt
				if !known || !isSameSnapshot(old, snap) {
					snap.UpdatedAt = now // Only a snapshot whose content changed or that is new gets a new time
				}
				switch {
				case old.UnavailableSince != "":
					revived = append(revived, snap.Name)
				case known && old.Name != snap.Name:
					renamed = append(renamed, old.Name+" -> "+snap.Name)
				case known && !isSameSnapshot(old, snap):
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
		a.RepoMeta = next
		return commitMessage, nil
	})
	if err != nil {
		return MetaRefreshResult{}, err
	}
	result := MetaRefreshResult{NumCheckedRepos: checked}
	for _, m := range updated.RepoMeta {
		if m.UnavailableSince != "" {
			result.UnavailableRepoIDs = append(result.UnavailableRepoIDs, m.ID)
		}
	}
	return result, nil
}
