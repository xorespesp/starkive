// The step that deploys the dashboard to Vercel.
// It picks and checks the commit to deploy to Vercel (`resolveVercelDeployCommit`: the commit this setup tool was built from, in the Vercel deploy repository).
// It builds the Vercel Deploy link (`vercel.com/new/clone`) with the dashboard name (`buildDashboardName`) and opens it.
// Then it checks the default address of that name (`https://<dashboard name>.vercel.app`) every few seconds, and moves to the next step when the Vercel deployment is done.
// If the check doesn't pass (for example, the account already has a repository with the same name, so Vercel gave another name), the person enters the address by hand.
package main

import (
	"context"
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

// A reference to one commit of a GitHub repository: the repository address and the full commit sha
type commitRef struct {
	url string // Address of the repository, `https://github.com/<owner>/<repo>`
	sha string // Commit sha (40 hex characters)
}

// Builds the address of this commit's code on GitHub, `<url>/tree/<sha>`.
// The Vercel Deploy link copies the code at this address, and the setup tool shows it before it starts.
//
// NOTE: The release workflow runs the setup tool with `-version` and looks for this address in the output (`.github/workflows/release.yml`).
// So if the form changes, change the workflow too.
// The method has a value receiver `(c commitRef)`: it gets a copy of the struct, so changes inside don't reach the caller (similar in effect to a `const` member function in C++).
func (c commitRef) buildTreeURL() string {
	return c.url + "/tree/" + c.sha
}

// Reports whether `sha` is a full commit sha (40 hex characters)
func isValidCommitSHA(sha string) bool {
	_, err := hex.DecodeString(sha)
	return err == nil && len(sha) == 40
}

// Picks the commit to deploy to Vercel: the commit this setup tool was built from, so the setup tool and the dashboard run the same code.
//   - Release executable: the repository is `stampedVercelDeployRepoURL`, and the commit is the one Go stamped (`readStampedVCS`).
//     It doesn't run git, so it works in any folder
//   - Executable from a plain `go build`: the commit is the one Go stamped, and the repository is the `origin` of the folder it runs in
//   - No stamped commit (`go run`): it reads `origin` and `HEAD` of the folder it runs in.
//     `go run` builds from that folder, so `HEAD` is the code that runs
//
// Returns:
//   - `hasUncommittedChanges`: The changes are in the running code, but not in the deployed commit
//   - `err`: An error if the release build is broken (a wrong repository address, or no stamped commit), or the reason git can't read the folder
func resolveVercelDeployCommit() (vercelDeployCommit commitRef, hasUncommittedChanges bool, err error) {
	stampedSHA, stampedHasChanges, _ := readStampedVCS()

	// Release executable: the stamped repository and commit.
	// A broken build stops here instead of reading git
	if stampedVercelDeployRepoURL != "" {
		fullName, found := strings.CutPrefix(stampedVercelDeployRepoURL, "https://github.com/")
		if !found || !core.IsValidRepoFullName(fullName) || stampedSHA == "" {
			return commitRef{}, false, fmt.Errorf("this setup tool was built wrongly (Vercel deploy repository %q, stamped commit %q). Download the latest setup tool", stampedVercelDeployRepoURL, stampedSHA)
		}
		return commitRef{url: stampedVercelDeployRepoURL, sha: stampedSHA}, stampedHasChanges, nil
	}

	// Otherwise the repository is the `origin` of the folder it runs in
	originRemote, err := runGit("remote", "get-url", "origin")
	if errors.Is(err, exec.ErrNotFound) { // `errors.Is`: checks wrapped errors too, to see if it is that value
		return commitRef{}, false, errors.New("git isn't installed. Install Git, clone starkive with it, and run the setup tool in the clone")
	}
	if err != nil {
		return commitRef{}, false, errors.New("this folder isn't a git clone of starkive (or it has no origin). Clone starkive with git and run the setup tool in the clone")
	}
	fullName, err := parseGitRemoteURL(originRemote)
	if err != nil {
		return commitRef{}, false, err
	}
	originURL := "https://github.com/" + fullName

	// Executable from a plain `go build`: the stamped commit
	if stampedSHA != "" {
		return commitRef{url: originURL, sha: stampedSHA}, stampedHasChanges, nil
	}

	// `go run`: the current commit of the folder, and whether its working tree has uncommitted changes
	headSHA, err := runGit("rev-parse", "HEAD")
	if err != nil {
		return commitRef{}, false, fmt.Errorf("couldn't read the current commit: %w", err)
	}
	if !isValidCommitSHA(headSHA) {
		return commitRef{}, false, fmt.Errorf("git returned an unexpected commit %q", headSHA)
	}
	changedFiles, err := runGit("status", "--porcelain") // One line per changed file. Empty string if none
	if err != nil {
		return commitRef{}, false, err
	}
	return commitRef{url: originURL, sha: headSHA}, changedFiles != "", nil
}

// Runs a git command with the arguments `args`, and returns its output with leading and trailing spaces removed.
// If git isn't installed, the error wraps `exec.ErrNotFound`
func runGit(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(out)), err
}

// Turns the git remote address `remote` into a GitHub repository name (`owner/repo`).
// It accepts `https://[user@]github.com/o/r`, `ssh://git@[ssh.]github.com[:port]/o/r` and `git@github.com:o/r`, with or without a trailing `.git` or `/`.
// The host is not case sensitive.
// It is an error if the host isn't GitHub or the path isn't in `owner/repo` form
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
		return "", fmt.Errorf("the origin of this clone (%s) isn't a GitHub repository. The Vercel Deploy link can only copy a public GitHub repository", remote)
	}
	name := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !core.IsValidRepoFullName(name) {
		return "", fmt.Errorf("the origin of this clone (%s) isn't a repository address like https://github.com/owner/repo", remote)
	}
	return name, nil
}

// Asks GitHub whether the Vercel Deploy link can copy the commit to deploy to Vercel, `vercelDeployCommit`.
// The Vercel deploy repository must be public, and the commit must be on GitHub.
// It asks without authentication (it deals only with public repositories, and it runs before the setup token is entered).
//
// Args:
//   - `apiBase`: Address of the GitHub API (a fake server in tests)
//   - `vercelDeployCommit`: The commit picked by `resolveVercelDeployCommit`
//
// NOTE: The Vercel Deploy link can't copy a private repository (measured).
// It can't fetch a commit that isn't on GitHub (not pushed).
// If the copy fails, only an empty repository with the dashboard name is left (measured).
// So the setup stops here, before the person creates a repository and a token.
// Without authentication, a private repository gives the same 404 as a repository that doesn't exist.
func checkVercelDeployCommit(ctx context.Context, apiBase string, vercelDeployCommit commitRef) error {
	isStampedBuild := stampedVercelDeployRepoURL != "" // Release executables are stamped, so the user can't fix this with git
	fullName := strings.TrimPrefix(vercelDeployCommit.url, "https://github.com/")
	status, err := readPublicGitHubStatus(ctx, apiBase+"/repos/"+fullName)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		advice := "run the setup tool in a clone of a public starkive repository"
		if isStampedBuild {
			advice = "download the latest setup tool"
		}
		return fmt.Errorf("couldn't find %s as a public repository on GitHub. The Vercel Deploy link can only copy a public repository: %s", vercelDeployCommit.url, advice)
	}
	status, err = readPublicGitHubStatus(ctx, apiBase+"/repos/"+fullName+"/commits/"+vercelDeployCommit.sha)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity { // A commit that isn't in the repository (not pushed) gives 422 (measured)
		if isStampedBuild {
			return fmt.Errorf("commit %s is no longer on GitHub in %s. Download the latest setup tool", vercelDeployCommit.sha[:7], vercelDeployCommit.url)
		}
		return fmt.Errorf("commit %s isn't on GitHub in %s yet. Push it (git push) and run the setup tool again from that clone", vercelDeployCommit.sha[:7], vercelDeployCommit.url)
	}
	return nil
}

// Sends a GET to the GitHub API address `target` without authentication and returns the status code.
// For an answer other than 200, 404 or 422, it returns GitHub's error as is
func readPublicGitHubStatus(ctx context.Context, target string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
	return 0, core.CheckGitHubResponse(res)
}

// Suffix of the dashboard name.
// It is the DB repository name suffix (`dbRepoSuffix`) without `-db`, so the two look like a pair
const dashboardSuffix = "-starkive"

// Maximum length of the dashboard name.
// The name becomes the first label of the address (`<name>.vercel.app`), and DNS allows up to 63 characters per label
const maxDashboardNameLength = 63

// Builds the dashboard name.
// It is the name of the copy repository that the Vercel Deploy link creates in the user's account.
// Vercel uses this name as is for the project name and the default address `https://<name>.vercel.app`.
// (Measured: the copy page has no project name field, and the link's `project-name` is not used.)
//   - If the archive name is the same as the GitHub username: `<archive name>-starkive`
//   - Otherwise: `<username>-<archive name>-starkive`. If it is longer than 63 characters, the archive name part is cut
//
// Args:
//   - `owner`: GitHub username of the token owner (letters, digits and `-`, up to 39 characters, docs)
//   - `archiveName`: The archive name from step 1 (`normalizeArchiveName`)
//
// NOTE: A default address is unique across all of Vercel (measured).
// If the name is already taken, Vercel gives another address, and the later check (`watchVercelDeployment`) doesn't notice the deployment.
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

// Interval between checks of whether the Vercel deployment is done
const vercelDeploymentPollInterval = 5 * time.Second

// Builds the address of the Vercel Deploy link (the page that creates a project).
//
// Args:
//   - `vercelDeployCommit`: The commit to copy and deploy (`resolveVercelDeployCommit`). The link points to that commit (`/tree/<commit sha>`), so Vercel copies exactly that commit (measured)
//   - `name`: The dashboard name (`buildDashboardName`). It fills the copy repository name field, and becomes the project name and the default address
//
// NOTE: The link can fill in only the names of environment variables, not their values (docs: the address stays in the browser history).
// The person copies the value from the setup tool and pastes it.
// Vercel's done page doesn't follow the return address of this link (measured).
// So `watchVercelDeployment` checks the address itself to know when the Vercel deployment is done.
func buildVercelDeployURL(vercelDeployCommit commitRef, name string) string {
	query := url.Values{
		"repository-url":  {vercelDeployCommit.buildTreeURL()},
		"repository-name": {name},
		"env":             {core.DBRepoAccessKeyEnv},
		"envDescription":  {"Paste the value the starkive setup tool shows."},
	}
	return "https://vercel.com/new/clone?" + query.Encode()
}

// Checks the dashboard address `site` again and again in the background, until it is confirmed as this setup's dashboard (`checkDashboard`).
//
// Args:
//   - `cfg`: The DB repository ID and token being set up. It checks that the deployment uses these values
//   - `site`: The dashboard address to check
//
// Returns:
//   - A channel that receives the address once, when it is confirmed
//   - A function that stops the check. The check goes on until it is called. It is safe to call more than once
//
// NOTE: Before the Vercel deployment is done, the check normally fails: the address doesn't exist yet (404), or it is still deployed with another value.
// So failures are not shown, and it checks again.
// How long the person takes to finish the Vercel deployment is unknown, so it checks until told to stop (one request every 5 seconds is no burden).
func watchVercelDeployment(cfg *core.EnvConfig, site string) (<-chan string, func()) {
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
			case <-time.After(vercelDeploymentPollInterval):
			case <-stop:
				return
			}
		}
	}()
	return confirmed, func() { stopOnce.Do(func() { close(stop) }) }
}
