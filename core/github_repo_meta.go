// Reading the current metadata of repositories from GitHub (`FetchRepoMetaByNodeIDs`),
// and turning one repository of that answer into the fields of a snapshot (`buildRepoMeta`).
// The snapshots that keep it as a record in the archive are in `archive_repo_meta.go`.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Largest number of IDs that one GraphQL `nodes(ids:)` call can ask for (docs)
const MaxRepoMetaNodeIDs = 100

// Asks GitHub for the current info of many repositories at once by `node_id`.
//
// Args:
//   - `ids`: `node_id`s of the repositories to ask about. At most `MaxRepoMetaNodeIDs`
//
// Returns:
//   - Repository info in the same order and length as `ids` (raw JSON from GraphQL). `nil` where nothing was found
//   - IDs that GitHub clearly answered as "not found" (`NOT_FOUND`), such as deleted or made private
//   - An error if the request itself failed. Then no repository may be judged as gone
//
// NOTE: If some IDs can't be accessed, GraphQL doesn't fail the whole answer.
// It puts `null` in their place, and writes the reason and position in its errors (docs).
// That partial result is used, so "request failed" and "this repository doesn't exist" can be told apart.
// A temporary outage isn't recorded as a disappearance.
func FetchRepoMetaByNodeIDs(ctx context.Context, cfg *EnvConfig, ids []string) ([]json.RawMessage, []string, error) {
	// Star count, fork count and last push time are read only for display in the dashboard
	const query = `query($ids: [ID!]!) {
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
	var data struct {
		Nodes []json.RawMessage `json:"nodes"` // Each repository is kept raw, not decoded (it is passed to the dashboard as it is)
	}
	err := callGraphQL(ctx, cfg, query, map[string]any{"ids": ids}, &data)
	var nodeErrs graphQLErrors // Errors about some repositories, when the rest of the answer can be used
	if err != nil && !errors.As(err, &nodeErrs) {
		return nil, nil, err
	}
	if len(data.Nodes) != len(ids) { // If positions don't line up, we can't tell which result belongs to which repository
		return nil, nil, Errorf(http.StatusBadGateway, "GitHub GraphQL returned %d nodes for %d ids", len(data.Nodes), len(ids))
	}

	nodes := make([]json.RawMessage, len(ids))
	missing := []string{}
	for i, node := range data.Nodes {
		switch {
		case string(node) != "null" && string(node) != "{}": // `{}` is an ID of another kind, not a repository
			nodes[i] = node
		case string(node) == "null" && nodeErrs.isNodeNotFound(i): // Other errors aren't treated as a disappearance
			missing = append(missing, ids[i])
		}
	}
	return nodes, missing, nil
}

// Copies raw repository info from GraphQL into the repository info fields of a snapshot.
//
// Args:
//   - `node`: Raw JSON of one repository returned by `FetchRepoMetaByNodeIDs`
//
// Returns:
//   - A snapshot with only the repository info filled in. The caller fills in the ID and the time fields
func buildRepoMeta(node json.RawMessage) (RepoMeta, error) {
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
