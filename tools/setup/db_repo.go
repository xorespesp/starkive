// Steps that prepare the DB repository.
// At the start of the setup, it initializes an empty repository (writes the marker) and adds the initial files.
// At the end of the setup, it records the dashboard address and installs the workflow.
// A small helper that asks GitHub for JSON (`getGitHubJSON`) is here too.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Writes the marker (`core.MarkerDBPath`, with the format version) to an empty DB repository as the first commit.
// This makes it a starkive repository.
// `cfg` must already have the repository name and default branch from `core.ResolveDBRepo`.
//
// Returns:
//   - An error that says "run it again" if the first commit never shows up when reading the branch.
//     The marker is already written, so a new run sees a starkive repository and goes on from there
//
// NOTE: On an empty repository, the Git Data API (reading a branch, creating a tree) answers 409 (measured).
// So only this first commit is written with the Contents API.
// Right after the write, reading the branch may not show the first commit for a short time (read delay after a write, measured).
// It waits until the commit shows up, so the next request that reads the branch doesn't fail.
func initializeDBRepo(cfg *core.Config) error {
	body := map[string]string{
		"message": "first commit",
		"content": base64.StdEncoding.EncodeToString(core.EncodeMarker()), // The Contents API takes file content as base64
		"branch":  cfg.DBRepoBranch,
	}
	res, err := core.CallGitHubAPI(cfg, core.BuildContentsPath(cfg, core.MarkerDBPath), core.GitHubAPIRequest{Method: http.MethodPut, Body: body})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := core.CheckGitHubAPI(res); err != nil {
		return err
	}
	// Right after the write, reading the branch may not show the first commit yet.
	// It reads up to 5 times, and waits 0.3, 0.6, ... 1.5 seconds after each failed read
	for attempt := 1; attempt <= 5; attempt++ {
		res, err := core.CallGitHubAPI(cfg, "/repos/"+cfg.DBRepoFullName+"/git/ref/heads/"+cfg.DBRepoBranch, core.GitHubAPIRequest{})
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(time.Duration(attempt) * 300 * time.Millisecond)
	}
	return errors.New("GitHub hasn't shown the first commit yet. Wait a moment and run the setup tool again")
}

// Guide file shown on the front page of the DB repository.
// It is added only when the repository has no README. An existing README is left as it is (`addInitialFiles`)
const (
	dbRepoReadmePath    = "README.md" // Path in the repository
	dbRepoReadmeContent = `# Starkive Archive Repository

This repository stores the archive of your [starkive](https://github.com/xorespesp/starkive) dashboard: the repositories you saved, your lists and your notes.

- Keep this repository private unless you want your archive to be public.
- Don't edit, move or delete the files here, or the STARKIVE_SYNC_KEY secret in Settings. The dashboard and the starkive setup tool manage them.
- Turn auto sync on or off in the dashboard's Settings, not in the Actions tab.
- Deleting this repository deletes your archive.
`
)

// Path of the empty file that keeps the data folder (`db/`) in the DB repository before the first data file is written.
// Git doesn't store empty folders, so without it the folder doesn't show on GitHub
var dbFolderKeepPath = path.Dir(core.StarItemsDBPath) + "/.gitkeep"

// Adds the initial files that are missing from the DB repository, in one commit.
// If none is missing, it doesn't commit.
// A file that is already there isn't overwritten, because the user may have edited it.
//
// NOTE: The first commit of an empty repository can hold only the marker.
// GitHub refuses to make a tree or a commit with several files in an empty repository (Git Data API 409, GraphQL "Branch not found", measured).
// So these files come in the next commit.
// It runs on every setup, not only right after initialization.
// So if the setup stopped between the two commits, running it again adds them.
func addInitialFiles(cfg *core.Config) error {
	paths := []string{dbRepoReadmePath, dbFolderKeepPath}
	return core.UpdateFiles(cfg, paths, func(original map[string][]byte) (map[string][]byte, core.CommitMessage, error) {
		written := map[string][]byte{} // Files to write in this commit: path -> content
		if original[dbRepoReadmePath] == nil {
			written[dbRepoReadmePath] = []byte(dbRepoReadmeContent)
		}
		if original[dbFolderKeepPath] == nil {
			written[dbFolderKeepPath] = []byte{}
		}
		return written, core.NewCommitMessage("feat", "Add initial files"), nil
	})
}

// The last step of the setup.
// Records the dashboard address in `config.json`, and installs the auto sync workflow.
// Only when the workflow is installed for the first time, it writes to the config file that auto sync is on (`core.RecordSyncEnabled`).
//
// Args:
//   - `site`: The dashboard address registered in the sign-in GitHub App (the dashboard's canonical address).
//     The workflow calls this address every day
//
// NOTE: The address is recorded first, and the workflow is installed after.
// This way the workflow has an address to call from its first run.
// The workflow can then always treat a config file it can't read as a failure, not as "not set up yet".
// If the workflow already exists, the on/off setting isn't touched.
// This keeps a new run of the setup tool from turning on what the person turned off in Settings.
func finishDBRepo(cfg *core.Config, site string) error {
	if err := core.SetDashboardURL(cfg, site); err != nil {
		return err
	}
	workflow, err := core.ReadFile(cfg, core.SyncWorkflowPath)
	if err != nil {
		return err
	}
	if workflow == nil { // First install: GitHub registers a new workflow as turned on (measured), so the setting is set to on too
		if err := core.RecordSyncEnabled(cfg); err != nil {
			return err
		}
	}
	return installWorkflow(cfg)
}

// Sends a GET to the GitHub API and decodes the JSON response into `dst`.
// For a failure response, it returns the error from `core.CheckGitHubAPI`.
//
// Args:
//   - `path`: API path (e.g. `/repos/owner/repo`)
//   - `dst`: Pointer to the value to decode the response into
func getGitHubJSON(cfg *core.Config, path string, dst any) error {
	res, err := core.CallGitHubAPI(cfg, path, core.GitHubAPIRequest{})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if err := core.CheckGitHubAPI(res); err != nil {
		return err
	}
	return json.NewDecoder(res.Body).Decode(dst)
}
