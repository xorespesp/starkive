// Archive entries (`db/star-items.json`, `StarItem`), and putting GitHub Stars into the archive as new entries (`ArchiveStarredRepos`).
// The archive as a whole is in `archive.go`, and reading Stars from GitHub is in `github_stars.go`.
package core

import (
	"context"
	"slices"
)

// One archive entry (one line of `db/star-items.json`).
// The field order is the key order in the file.
// Changing the order turns the whole file into a diff.
//
// NOTE: Values that the repository owner can change over time (description, language, Star count, etc.) are not stored here.
// Stored copies would drift from the real values.
// The page fetches such values fresh from `/api/github-repo-meta` each time, and the snapshot for the record lives in `RepoMeta`.
type StarItem struct {
	ID          int64   `json:"id"`                // GitHub repository ID. It stays the same when the name changes, so entries are identified by it
	NodeID      string  `json:"node_id,omitempty"` // ID for GraphQL queries (new format `R_kgDO...`)
	Name        string  `json:"name"`              // `owner/repo` at the time it was archived. Shown only when the repository is gone and can't be fetched
	Note        string  `json:"note"`              // The user's note
	StarListIDs []int64 `json:"star_list_ids"`     // IDs of the star lists it belongs to. Always cleaned by `Archive.CleanStarListIDs`
	StarredAt   string  `json:"starred_at"`        // When it was starred on GitHub. For entries added by hand, when it was added
}

// Puts GitHub Stars at the front of the archive as new items.
// Every path that puts Stars into the archive (sync, Import) goes through this function.
//   - Skips repositories already in the archive (by ID), and repositories repeated within `starredRepos`.
//     Existing items are left as they are
//   - Puts new items in the star lists of `starListIDs` and in the star lists they belonged to on GitHub.
//     If the archive has no star list with the same name, one is made
//   - New star lists and new items are written together in one commit.
//     The commit subject is `<commitPrefix>: archive N stars`.
//     The body lists the added repositories and the names of new star lists
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
func ArchiveStarredRepos(ctx context.Context, repo *DBRepo, starredRepos []StarredRepo, starListIDs []int64, commitPrefix string) (int, error) {
	// GitHub star lists are read only when there are new repositories to add (most daily syncs have none).
	// They aren't read again when `change` is called again after a conflict.
	// They are input from GitHub, not the archive, so another commit doesn't make them stale
	var githubStarLists map[int64][]string
	added := 0
	_, err := UpdateArchive(ctx, repo, func(a *Archive) (CommitMessage, error) {
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
			if githubStarLists, err = FetchGitHubStarListsByRepo(ctx, repo.cfg); err != nil {
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
