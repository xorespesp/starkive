// The step that deploys the dashboard to Vercel.
// It picks and checks the commit to deploy (`sourceRepoCommit`: the `origin` of this clone, which is the source repository, and its current commit).
// It builds the Deploy link (`vercel.com/new/clone`) with the dashboard name (`buildDashboardName`) and opens it.
// Then it checks the default address of that name (`https://<dashboard name>.vercel.app`) every few seconds, and moves to the next step when the deployment is done.
// If the check doesn't pass (for example, the account already has a repository with the same name, so Vercel gave another name), the person enters the address by hand.
package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/xorespesp/starkive/core"
)

// One commit of the source repository.
// The source repository is the repository that the Deploy link copies into the user's account (the original starkive or a fork of it).
// This commit is deployed as the dashboard.
// It is not the same as `source` in the GitHub API (the top original of a fork)
type sourceRepoCommit struct {
	url string // Address of the source repository, `https://github.com/<owner>/<repo>`
	sha string // Commit sha (40 hex characters)
}

// Reads the commit to deploy from the clone that runs the setup tool: the current commit `HEAD` of the source repository `origin`.
//
// Returns:
//   - The commit to deploy
//   - `true` if the working tree has uncommitted changes. Those changes are not part of the deployment
//   - An error with the reason if git isn't installed, this folder isn't a git clone (for example, downloaded as a ZIP), or `origin` isn't a GitHub repository
//
// NOTE: The Deploy link copies this commit into a new repository in the user's account and deploys it (`buildDeployURL`).
// So the setup tool and the dashboard run the same code.
func readSourceRepoCommit() (sourceRepoCommit, bool, error) {
	remote, err := runGit("remote", "get-url", "origin")
	if errors.Is(err, exec.ErrNotFound) { // `errors.Is`: checks wrapped errors too, to see if it is that value
		return sourceRepoCommit{}, false, errors.New("git isn't installed. Install Git, clone starkive with it, and run the setup tool in the clone")
	}
	if err != nil {
		return sourceRepoCommit{}, false, errors.New("this folder isn't a git clone of starkive (or it has no origin). Clone starkive with git and run the setup tool in the clone")
	}
	fullName, err := parseGitRemoteURL(remote)
	if err != nil {
		return sourceRepoCommit{}, false, err
	}
	sha, err := runGit("rev-parse", "HEAD")
	if err != nil {
		return sourceRepoCommit{}, false, fmt.Errorf("couldn't read the current commit: %w", err)
	}
	if _, err := hex.DecodeString(sha); err != nil || len(sha) != 40 {
		return sourceRepoCommit{}, false, fmt.Errorf("git returned an unexpected commit %q", sha)
	}
	changes, err := runGit("status", "--porcelain") // One line per changed file. Empty string if none
	if err != nil {
		return sourceRepoCommit{}, false, err
	}
	return sourceRepoCommit{url: "https://github.com/" + fullName, sha: sha}, changes != "", nil
}

// Runs a git command with the arguments `args`, and returns its output with leading and trailing spaces removed.
// If git isn't installed, the error wraps `exec.ErrNotFound`
func runGit(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// Turns the git remote address `remote` into a GitHub repository name (owner/repo).
// It accepts `https://[user@]github.com/o/r`, `ssh://git@[ssh.]github.com[:port]/o/r` and `git@github.com:o/r`, with or without a trailing `.git` or `/`.
// The host is not case sensitive.
// It is an error if the host isn't GitHub or the path isn't in owner/repo form
func parseGitRemoteURL(remote string) (string, error) {
	address := strings.TrimSpace(remote)
	if !strings.Contains(address, "://") { // An SSH address written as `host:path` without a scheme, like `git@github.com:o/r`
		if host, path, ok := strings.Cut(address, ":"); ok {
			address = "ssh://" + host + "/" + path
		}
	}
	u, err := url.Parse(address)
	host := ""
	if err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host != "github.com" && host != "www.github.com" && host != "ssh.github.com" {
		return "", fmt.Errorf("the origin of this clone (%s) isn't a GitHub repository. The Deploy link can only copy a public GitHub repository", remote)
	}
	name := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !core.IsValidRepoFullName(name) {
		return "", fmt.Errorf("the origin of this clone (%s) isn't a repository address like https://github.com/owner/repo", remote)
	}
	return name, nil
}

// Asks GitHub whether the Deploy link can copy the commit to deploy, `commit`.
// The source repository must be public, and the commit must be on GitHub.
// It asks without authentication (it deals only with public repositories, and it runs before the setup token is entered).
//
// Args:
//   - `apiBase`: Address of the GitHub API (a fake server in tests)
//   - `commit`: The commit read by `readSourceRepoCommit`
//
// NOTE: The Deploy link can't copy a private repository (measured).
// It can't fetch a commit that isn't on GitHub (not pushed).
// If the copy fails, only an empty repository with the dashboard name is left (measured).
// So the setup stops here, before the person creates a repository and a token.
// Without authentication, a private repository gives the same 404 as a repository that doesn't exist.
func checkSourceRepoCommit(apiBase string, commit sourceRepoCommit) error {
	fullName := strings.TrimPrefix(commit.url, "https://github.com/")
	status, err := readPublicGitHubStatus(apiBase + "/repos/" + fullName)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("couldn't find %s as a public repository on GitHub. The Deploy link can only copy a public repository: "+
			"run the setup tool in a clone of the original starkive or of your fork", commit.url)
	}
	status, err = readPublicGitHubStatus(apiBase + "/repos/" + fullName + "/commits/" + commit.sha)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity { // A commit that isn't in the repository (not pushed) gives 422 (measured)
		return fmt.Errorf("commit %s isn't on GitHub in %s yet. Push it (git push) and run the setup tool again", commit.sha[:7], commit.url)
	}
	return nil
}

// Sends a GET to the GitHub API address `target` without authentication and returns the status code.
// For an answer other than 200, 404 or 422, it returns GitHub's error as is
func readPublicGitHubStatus(target string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "starkive-setup")
	res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK, http.StatusNotFound, http.StatusUnprocessableEntity:
		return res.StatusCode, nil
	}
	return 0, core.CheckGitHubAPI(res)
}

// Suffix of the dashboard name.
// It is the DB repository name suffix (`dbRepoSuffix`) without `-db`, so the two look like a pair
const dashboardSuffix = "-starkive"

// Maximum length of the dashboard name.
// The name becomes the first label of the address (`<name>.vercel.app`), and DNS allows up to 63 characters per label
const maxDashboardNameLength = 63

// Builds the dashboard name.
// It is the name of the copy repository that the Deploy link creates in the user's account.
// Vercel uses this name as is for the project name and the default address `https://<name>.vercel.app`.
// (Measured: the copy page has no project name field, and the link's `project-name` is not used.)
//   - If the archive name is the same as the GitHub username: `<archive name>-starkive`
//   - Otherwise: `<username>-<archive name>-starkive`. If it is longer than 63 characters, the archive name part is cut
//
// Args:
//   - `owner`: GitHub username of the token owner (letters, digits and `-`, up to 39 characters)
//   - `archiveName`: The archive name from step 1 (`normalizeArchiveName`)
//
// NOTE: A default address is unique across all of Vercel.
// If the name is already taken, Vercel gives another address, and the later check (`watchDeployment`) doesn't notice the deployment.
// The name always starts with the GitHub username (unique across GitHub), so it doesn't clash with other users.
func buildDashboardName(owner string, archiveName string) string {
	owner = strings.ToLower(owner)
	if archiveName == owner {
		return archiveName + dashboardSuffix
	}
	room := maxDashboardNameLength - len(owner) - 1 - len(dashboardSuffix)    // Even with a 39-character username, 14 characters are left
	word := strings.TrimRight(archiveName[:min(len(archiveName), room)], "-") // If the cut ends with a hyphen, drop it so it doesn't become `--`
	return owner + "-" + word + dashboardSuffix
}

// Interval between checks of whether the deployment is done
const deploymentPollInterval = 5 * time.Second

// Builds the address of Vercel's Deploy link (the page that creates a project).
//
// Args:
//   - `commit`: The commit to copy and deploy (`readSourceRepoCommit`). The link points to that commit (`/tree/<commit sha>`), so Vercel copies exactly that commit (measured)
//   - `name`: The dashboard name (`buildDashboardName`). It fills the copy repository name field, and becomes the project name and the default address
//
// NOTE: The link can fill in only the names of environment variables, not their values (docs: the address stays in the browser history).
// The person copies the value from the setup tool and pastes it.
// Vercel's done page doesn't follow the return address of this link (measured).
// So `watchDeployment` checks the address itself to know when the deployment is done.
func buildDeployURL(commit sourceRepoCommit, name string) string {
	query := url.Values{
		"repository-url":  {commit.url + "/tree/" + commit.sha},
		"repository-name": {name},
		"env":             {core.DBRepoAccessKeyEnv},
		"envDescription":  {"Paste the value the starkive setup tool shows."},
	}
	return "https://vercel.com/new/clone?" + query.Encode()
}

// Checks the dashboard address `site` again and again in the background, until it is confirmed as this setup's dashboard (`checkDashboard`).
//
// Args:
//   - `cfg`: The DB repository and token being set up. It checks that the deployment uses these values
//   - `site`: The dashboard address to check
//
// Returns:
//   - A channel that receives the address once, when it is confirmed
//   - A function that stops the check. The check goes on until it is called. It is safe to call more than once
//
// NOTE: Before the deployment is done, the check normally fails: the address doesn't exist yet (404), or it is still deployed with another value.
// So failures are not shown, and it checks again.
// How long the person takes to finish the deployment is unknown, so it checks until told to stop (one request every 5 seconds is no burden).
func watchDeployment(cfg *core.Config, site string) (<-chan string, func()) {
	confirmed := make(chan string, 1) // Buffer of 1: the sender doesn't block even if the receiver isn't waiting yet
	stop := make(chan struct{})
	var stopOnce sync.Once // Makes sure `stop` is closed only once (closing a closed channel makes Go panic)

	go func() { // `go`: checks in a goroutine (a lightweight thread), so the person can be asked for an address meanwhile
		for {
			if checkDashboard(cfg, site) == nil {
				confirmed <- site
				return
			}
			select { // Waits until the next check, but ends if told to stop
			case <-time.After(deploymentPollInterval):
			case <-stop:
				return
			}
		}
	}()
	return confirmed, func() { stopOnce.Do(func() { close(stop) }) }
}
