// Content of the auto sync workflow put in the DB repository.
// Also its installation and the check of the permissions it needs (Workflows, Actions).
// Only this tool installs the workflow.
// The dashboard server only reads the workflow's state and run history through the Actions API, and turns the workflow on and off.
package main

import (
	"net/http"

	"github.com/xorespesp/starkive/core"
)

// Content of the DB repository's auto sync workflow (`core.SyncWorkflowPath`).
// Every day at 18:17 UTC, it reads `dashboard_url` from `config.json`.
// It then calls `GET <dashboard_url>/api/sync-archive`, with the sync key from the Actions secret (`core.SyncKeySecretName`) in `Authorization: Bearer`.
//
// NOTE: These are the contract with the server: the config key `dashboard_url`, the secret name, the request shape,
// and the response that counts as success (200 with `status` `not_due` or `synced`, the return values of `core.RunScheduledSync`).
// Raise the major version of `core.DBRepoFormat` for a contract change.
// Then a repository with a server and a workflow of different contracts stops with a clear error.
// Raise the minor version for a change that keeps the contract but installed workflows need, such as a script bug fix.
// This tool never replaces an installed workflow (`installWorkflowIfAbsent`).
// So such a change reaches installed workflows only through a format conversion.
// A change that only new installs need, such as a comment, doesn't raise it.
// (`TestSyncWorkflowChangeIsReviewed` reports that the content changed.)
// A run ends in success only when success is clearly confirmed.
// Everything else is a failure.
// With "only known failures fail", unexpected cases would end as success, and GitHub wouldn't send a failure notice.
// Such cases are a redirect to a sign-in page, a page that isn't the dashboard, a failed config file read, and a rejected key.
// It uses no external action, only the GitHub CLI, `jq` and `curl` in the runner.
// The script was checked case by case for success and failure on Linux, with a fake `gh` and a fake dashboard (measured).
const syncWorkflowContent = `# Calls your starkive dashboard once a day so it archives new stars and refreshes repository snapshots.
#
# The dashboard does the work. This workflow only knocks on its door: it reads the dashboard address
# from config.json and calls /api/sync-archive with the key in the STARKIVE_SYNC_KEY secret,
# so the dashboard accepts the call from this workflow and no one else. The starkive setup tool stores both.
# The dashboard decides whether a sync is due, based on the interval you choose in its Settings,
# so you never need to edit this file. Turning auto sync on or off in the dashboard's Settings turns this workflow on or off.
#
# If a call fails, this run fails and GitHub notifies you, so a stopped sync doesn't go unnoticed.
# To sync right away, use Sync now in the dashboard's Settings. Running this workflow by hand syncs only on a due day,
# or when the last scheduled run failed.
name: starkive sync

on:
  schedule:
    - cron: '17 18 * * *' # every day at 18:17 UTC. GitHub Actions is busiest on the hour, so this avoids it
  workflow_dispatch:

permissions:
  contents: read # only reads config.json

concurrency:
  group: starkive-sync

jobs:
  sync:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - name: Call the dashboard
        env:
          GH_TOKEN: ${{ github.token }}
          REPO: ${{ github.repository }}
          SYNC_KEY: ${{ secrets.STARKIVE_SYNC_KEY }}
        run: |
          set -euo pipefail
          if [ -z "$SYNC_KEY" ]; then
            echo "::error::The STARKIVE_SYNC_KEY secret is missing. Run the starkive setup tool again"
            exit 1
          fi
          # The setup tool writes config.json before it installs this file, so failing to read it is always a failure.
          # Read from the default branch with the GitHub CLI that comes with the runner, so no third-party action runs here
          if ! config=$(gh api "repos/$REPO/contents/config.json" -H "Accept: application/vnd.github.raw+json"); then
            echo "::error::Couldn't read config.json"
            exit 1
          fi
          if ! url=$(printf '%s' "$config" | jq -er '.dashboard_url | select(type == "string" and startswith("https://"))'); then
            echo "::error::config.json has no https dashboard_url"
            exit 1
          fi
          # Only a reply from the dashboard itself counts as success: HTTP 200 with a status it knows.
          # Anything else fails this run, including a redirect to a sign-in page and a page that isn't the dashboard.
          # No retries: a failure must fail this run, so that GitHub notifies you
          body=$(mktemp)
          code=$(curl --silent --show-error --max-time 300 --output "$body" --write-out '%{http_code}' \
            -H "Authorization: Bearer $SYNC_KEY" "$url/api/sync-archive")
          status=$(jq -r '.status? // empty' "$body" 2>/dev/null || true)
          case "$code $status" in
            "200 not_due" | "200 synced")
              echo "starkive: $status"
              ;;
            *)
              echo "::error::The dashboard didn't confirm the sync (HTTP $code)"
              head -c 1000 "$body"
              echo
              exit 1
              ;;
          esac
`

// Checks, without a commit, that the token has the Workflows write permission (the permission to write files under `.github/workflows/`).
// Without the permission, it returns GitHub's 403 error as is.
// (`core.CheckGitHubAPI` adds the needed permission `workflows=write` to the message.)
//
// NOTE: It only tries to create a tree that holds the workflow file.
// Without the permission, GitHub rejects it with 403 at the tree creation step (measured).
// The created tree isn't linked to any commit, so it doesn't affect the branch or the schedule.
// This way the permission is checked before the GitHub App is created,
// and the workflow itself can be installed last, after the dashboard address is recorded.
func checkWorkflowsPermission(cfg *core.Config) error {
	repo := "/repos/" + cfg.DBRepoFullName
	var ref struct {
		Object struct {
			SHA string `json:"sha"` // Commit at the tip of the default branch
		} `json:"object"`
	}
	if err := getGitHubJSON(cfg, repo+"/git/ref/heads/"+cfg.DBRepoBranch, &ref); err != nil {
		return err
	}
	var commit struct {
		Tree struct {
			SHA string `json:"sha"` // File tree of that commit. Used as the base of the new tree
		} `json:"tree"`
	}
	if err := getGitHubJSON(cfg, repo+"/git/commits/"+ref.Object.SHA, &commit); err != nil {
		return err
	}
	entries := []map[string]string{{"path": core.SyncWorkflowPath, "mode": "100644", "type": "blob", "content": syncWorkflowContent}}
	body := map[string]any{"base_tree": commit.Tree.SHA, "tree": entries}
	// Creates only a tree, not a commit. Without the Workflows permission, GitHub rejects it here with 403
	res, err := core.CallGitHubAPI(cfg, repo+"/git/trees", core.GitHubAPIRequest{Method: http.MethodPost, Body: body})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return core.CheckGitHubAPI(res)
}

// Checks that the token can read the DB repository's Actions (the workflow list and run history).
// With this permission, the dashboard reads the auto sync workflow's state and run history, and turns the workflow on and off.
// Without the permission, it returns GitHub's 403 error as is.
//
// NOTE: There is no way to check the write permission without side effects, so only reading is checked.
// The token creation link (`buildTokenURL`) asks for read and write (`actions=write`).
func checkActionsPermission(cfg *core.Config) error {
	var list struct{} // The response content isn't used. Only whether the read succeeds matters
	return getGitHubJSON(cfg, "/repos/"+cfg.DBRepoFullName+"/actions/workflows?per_page=1", &list)
}

// Installs the auto sync workflow (`syncWorkflowContent`) if the DB repository doesn't have it.
// It leaves a workflow that is already there as it is, even if its content differs.
// Without the permission, it returns GitHub's 403 error as is (the needed permission `workflows=write` is added to the message).
//
// NOTE: This tool doesn't change the code of a dashboard that is already deployed.
// The workflow must match that dashboard, so this tool leaves it as it is too.
func installWorkflowIfAbsent(cfg *core.Config) error {
	paths := []string{core.SyncWorkflowPath}
	return core.UpdateFiles(cfg, paths, func(original map[string][]byte) (map[string][]byte, core.CommitMessage, error) {
		if original[core.SyncWorkflowPath] != nil {
			return nil, core.CommitMessage{}, nil // Nothing to write, so no commit
		}
		written := map[string][]byte{core.SyncWorkflowPath: []byte(syncWorkflowContent)} // Files to write in this commit: path -> content
		message := core.NewCommitMessage("user(workflow)", "install auto sync workflow")
		return written, message, nil
	})
}
