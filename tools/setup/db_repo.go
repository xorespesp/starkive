// Steps that prepare the DB repository.
// At the start of the setup, it initializes an empty repository (writes the marker) and adds the initial files.
// At the end of the setup, it records the dashboard address and installs the workflow if it is missing.
// A small helper that asks GitHub for JSON (`getGitHubJSON`) is here too.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Writes the marker (`core.MarkerDBPath`, with the format version) to an empty DB repository as the first commit.
// This makes it a starkive repository.
//
// Returns:
//   - An error that says "run it again" if the first commit never shows up when reading the branch.
//     The marker is already written, so a new run sees a starkive repository and goes on from there
//
// NOTE: On an empty repository, the Git Data API (reading a branch, creating a tree) answers 409 (measured).
// So only this first commit is written with the Contents API.
// Right after the write, reading the branch may not show the first commit for a short time (read delay after a write, measured).
// It waits until the commit shows up in the same request that writes start with (`core.DBRepo.BuildBranchTipAPIPath`),
// so the next write doesn't fail.
func initializeDBRepo(ctx context.Context, dbRepo *core.DBRepo) error {
	body := map[string]string{
		"message": "first commit",
		"content": base64.StdEncoding.EncodeToString(core.EncodeDBRepoMarker()), // The Contents API takes file content as base64
		"branch":  dbRepo.Branch(),
	}
	res, err := core.CallGitHubAPI(ctx, dbRepo.EnvConfig(), dbRepo.BuildContentsAPIPath(core.MarkerDBPath), core.GitHubAPIRequest{Method: http.MethodPut, Body: body})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := core.CheckGitHubResponse(res); err != nil {
		return err
	}
	// Right after the write, reading the branch may not show the first commit yet.
	// It reads up to 5 times, and waits 0.3, 0.6, ... 1.2 seconds after each failed read but the last
	const maxReads = 5
	for attempt := 1; attempt <= maxReads; attempt++ {
		res, err := core.CallGitHubAPI(ctx, dbRepo.EnvConfig(), dbRepo.BuildBranchTipAPIPath(), core.GitHubAPIRequest{})
		if err == nil {
			var commits []json.RawMessage
			isShown := res.StatusCode == http.StatusOK && json.NewDecoder(res.Body).Decode(&commits) == nil && len(commits) > 0
			res.Body.Close()
			if isShown {
				return nil
			}
		}
		if attempt < maxReads {
			time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
		}
	}
	return errors.New("GitHub hasn't shown the first commit yet. Wait a moment and run the setup tool again")
}

// Path of the guide file shown on the front page of the DB repository.
const dbRepoReadmePath = "README.md"

// Content of the guide file shown on the front page of the DB repository.
const dbRepoReadmeContent = `# Starkive Archive Repository

This is your personal archive repository for the [starkive](https://github.com/xorespesp/starkive) project.

- Keep this repository private unless you want your archive to be public.
- Don't edit, move or delete the files here, or the ` + "`" + core.SyncKeySecretName + "`" + ` secret in Settings. The dashboard and the starkive setup tool manage them.
- Turn sync on or off in the dashboard's Settings, not in the Actions tab.
- Deleting this repository deletes your archive.
`

// Adds the initial files that are missing from the DB repository, in one commit.
// If none is missing, it doesn't commit.
// A file that is already there isn't overwritten, because the user may have edited it.
// It also adds the empty archive item shards to a new repository (`addEmptyStarItemShardsIfNewDBRepo`).
//
// NOTE: The first commit of an empty repository can hold only the marker.
// GitHub refuses to make a tree or a commit with several files in an empty repository (Git Data API 409, GraphQL "Branch not found", measured).
// So these files come in the next commit.
// It runs on every setup, not only right after initialization.
// So if the setup stopped between the two commits, running it again adds them.
func addInitialFiles(ctx context.Context, dbRepo *core.DBRepo) error {
	return core.UpdateDBRepoFiles(ctx, dbRepo, []string{dbRepoReadmePath}, func(filesAtCommit *core.DBRepoFilesAtCommit) (core.FileChangesCommit, error) {
		changes := core.FileChangesCommit{FilesToWrite: map[string][]byte{}, Message: core.NewCommitMessage("feat", "Add initial files")}
		if !filesAtCommit.HasFile(dbRepoReadmePath) {
			changes.FilesToWrite[dbRepoReadmePath] = []byte(dbRepoReadmeContent)
		}
		if err := addEmptyStarItemShardsIfNewDBRepo(ctx, dbRepo, filesAtCommit.CommitSHA(), &changes); err != nil {
			return core.FileChangesCommit{}, err
		}
		return changes, nil
	})
}

// Adds the empty archive item shards to `changes.FilesToWrite` if the DB repository at commit `commitSHA` has no data folder (`db/`) yet.
// That is a new repository.
// If it has one, every shard must be there, and none is filled in.
// `changes.FilesToWrite` must not be `nil` (writing to a `nil` map panics in Go).
//
// Returns:
//   - An error naming a missing shard if the data folder is there but a shard isn't (a broken repository, or a passing 404 of the listing).
//     Nothing is added then
//
// NOTE: A listing of the root tells "no data folder" for sure, unlike a 404 for one file, which can be a passing one.
// An empty shard written over a real one would lose its items.
// An empty shard is `[]` and a line break, the same bytes the server writes for no items.
// The server reads any JSON array, so a different layout would only be rewritten on the next change of that shard.
func addEmptyStarItemShardsIfNewDBRepo(ctx context.Context, dbRepo *core.DBRepo, commitSHA string, changes *core.FileChangesCommit) error {
	var root struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"` // `blob` for a file, `tree` for a folder
		} `json:"tree"`
	}
	if err := getGitHubJSON(ctx, dbRepo.EnvConfig(), dbRepo.BuildAPIPath("/git/trees/"+url.PathEscape(commitSHA)), &root); err != nil {
		return err
	}
	dataFolder := path.Dir(core.StarItemsDBDir) // `db`
	hasDataFolder := false
	for _, entry := range root.Tree {
		if entry.Path == dataFolder && entry.Type == "tree" {
			hasDataFolder = true
		}
	}
	if !hasDataFolder {
		for index := range core.StarItemShardCount {
			changes.FilesToWrite[core.BuildStarItemShardPath(index)] = []byte("[]\n")
		}
		return nil
	}

	var shardFolder struct {
		Tree []struct {
			Path string `json:"path"` // Relative to the folder
		} `json:"tree"`
	}
	err := getGitHubJSON(ctx, dbRepo.EnvConfig(), dbRepo.BuildAPIPath("/git/trees/"+url.PathEscape(commitSHA)+":"+core.StarItemsDBDir), &shardFolder)
	var githubErr *core.GitHubAPIError
	if err != nil && !(errors.As(err, &githubErr) && githubErr.Status == http.StatusNotFound) { // A folder that doesn't exist gives 404, the same as one with no shard
		return err
	}
	isListed := map[string]bool{} // File name in the folder -> `true`
	for _, entry := range shardFolder.Tree {
		isListed[entry.Path] = true
	}
	for index := range core.StarItemShardCount {
		shardPath := core.BuildStarItemShardPath(index)
		if !isListed[path.Base(shardPath)] {
			return fmt.Errorf("%s is missing in the DB repository. If GitHub answered slowly, run the setup again. If it is still missing, the repository is broken", shardPath)
		}
	}
	return nil
}

// The last step of the setup.
// Records the dashboard address in `config.json`, and installs the sync workflow if it is missing.
// Only when it installs the workflow, it writes to the config file that sync is on (`core.RecordSyncEnabled`).
//
// Args:
//   - `site`: The dashboard address registered in the sign-in GitHub App (the dashboard's canonical address).
//     The workflow calls this address every day
//
// NOTE: The address is recorded first, and the workflow is installed after.
// This way the workflow has an address to call from its first run.
// The workflow can then always treat a config file it can't read as a failure, not as "not set up yet".
// If the workflow already exists, the sync options aren't touched.
func finishDBRepo(ctx context.Context, dbRepo *core.DBRepo, site string) error {
	if err := core.SaveDashboardURL(ctx, dbRepo, site); err != nil {
		return err
	}
	// Checks whether this is the first install: an existing workflow means the setup ran before
	workflow, err := core.ReadDBRepoFile(ctx, dbRepo, core.SyncWorkflowPath)
	if err != nil {
		return err
	}
	if workflow != nil {
		return nil // Keeps the sync options the person chose
	}
	// Records sync as on in the sync options, because GitHub turns a new workflow on (measured).
	// This comes before the install.
	// Otherwise, if this failed after the install, the next run would see the workflow and never record it
	if err := core.RecordSyncEnabled(ctx, dbRepo); err != nil {
		return err
	}
	return installWorkflowIfAbsent(ctx, dbRepo)
}

// Sends a GET to the GitHub API and decodes the JSON response into `dst`.
// For a failure response, it returns the error from `core.CheckGitHubResponse`.
//
// Args:
//   - `path`: API path (e.g. `/repos/owner/repo`)
//   - `dst`: Pointer to the value to decode the response into
func getGitHubJSON(ctx context.Context, cfg *core.EnvConfig, path string, dst any) error {
	res, err := core.CallGitHubAPI(ctx, cfg, path, core.GitHubAPIRequest{})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := core.CheckGitHubResponse(res); err != nil {
		return err
	}
	return json.NewDecoder(res.Body).Decode(dst)
}
