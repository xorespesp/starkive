// The starkive setup tool.
// It puts the installation work a person would otherwise do by hand into one flow:
// preparing the DB repository, deploying the dashboard to Vercel with its environment variable value, the sign-in GitHub App, and the auto sync workflow.
// All of this runs on the user's computer, so the dashboard server has no setup path that is open before sign-in.
//
// Usage.
// The commit to deploy to Vercel is picked by `resolveVercelDeployCommit`.
// A release executable has the Vercel deploy repository address stamped at build time (`stampedVercelDeployRepoURL`, with `-ldflags -X`),
// and Go stamps the commit it was built from into the executable.
// So a release executable doesn't need git, and runs in any folder.
// Otherwise, run it in a folder where starkive was cloned with git.
// The Vercel Deploy link copies the commit the tool was built from (with `go run`, the current commit) from this clone's `origin`.
// So the dashboard runs the same code as the tool.
// For this, `origin` must be a public GitHub repository, and that commit must be pushed:
//
//	go run ./tools/setup              Runs on the setup page (browser)
//	go run ./tools/setup -headless    Runs in the terminal
//
// Steps (`run`, numbers and names are in `setupSteps`).
// Before it starts, it checks the commit to deploy to Vercel (`resolveVercelDeployCommit`, `checkVercelDeployCommit`).
// Each step skips work that is already done, so running it again continues from wherever it stopped:
//  1. Create archive repository: Takes the archive name (`normalizeArchiveName`).
//     It opens the page that creates the empty DB repository `<name>-starkive-db` (`buildNewDBRepoURL`),
//     then the token creation page filled in for that repository (`buildTokenURL`), and takes the token.
//     The setup looks for the repository by this name, so the user creates it without changing the name.
//     It looks for the repository in the token owner's account.
//     (A private repository can't be seen without a token, so the repository check comes after the token.)
//     It checks the repository ID and state, Star read access and Secrets access.
//     If the repository is empty, it writes the marker (`core.MarkerDBPath`) as the first commit and makes it a starkive repository.
//     If it is a starkive repository, it sets it up again.
//     If it is a foreign repository or has another format version, it stops.
//     Then it adds the initial files that are missing (`addInitialFiles`).
//     Next it checks the Workflows and Actions permissions (this needs a branch, so it runs after initialization).
//     Then it writes the scheduled sync key (`core.DeriveSyncKey`) to the DB repository's Actions secret (`core.SyncKeySecretName`)
//  2. Deploy dashboard: Checks that the deployment at the dashboard address is set up with this repository and the current token (`checkDashboard`).
//     The address is the one registered in the App, or the default address of the dashboard name (`buildDashboardName`) if there is no App.
//     If the check passes, it moves on.
//     Otherwise, if there is an App, it has the person change the environment variable `STARKIVE_DB_REPO_ACCESS_KEY` of that Vercel project and redeploy it.
//     If there is no App, it shows the Vercel Deploy link (`buildVercelDeployURL`) and the value.
//     While waiting for the Vercel deployment, it checks that address in the background and moves on by itself once it passes (`askDashboardURL`).
//     If the person chooses to enter it by hand, it stops the check and takes the address
//  3. Create sign-in app: If there is no App, or it can't be read, it creates the sign-in GitHub App
//  4. Install workflow: Records the dashboard address in `config.json`, and installs the auto sync workflow if it is missing
//
// For testing, the same environment variables as the server (`STARKIVE_GH_API_URL`, `STARKIVE_GH_WEB_URL`) can point the GitHub addresses to a fake server.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Names of the setup steps. The order is the step number (`ui.step` counts from 1)
var setupSteps = []string{"Create archive repository", "Deploy dashboard", "Create sign-in app", "Install workflow"}

// Suffix of the DB repository name.
// The repository name is `<archive name>-starkive-db`, and it pairs with the dashboard name suffix (`dashboardSuffix`)
const dbRepoSuffix = "-starkive-db"

// Maximum length of the archive name.
// It is the limit for GitHub usernames (39 characters), so anyone who enters their username as suggested can use it
const maxArchiveNameLength = 39

// Allowed shape of the archive name: lowercase letters and digits joined by single `-` (no `-` at either end).
// It is the same shape as a GitHub username.
// The dashboard name becomes one label of the address (`<name>.vercel.app`), so `.` and `_`, which can't be used there, are not accepted
var archiveNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Maximum length of the token name (the limit of GitHub's prefill `name`, docs)
const maxTokenNameLength = 40

// In setup page mode, the longest time the server stays open after the setup ends, so the page can fetch the last lines
const finishGracePeriod = 10 * time.Second

// Normalizes the archive name `input` that the person entered: removes leading and trailing spaces and makes it lowercase.
// If it has characters that aren't allowed (`archiveNamePattern`) or is longer than 39 characters (`maxArchiveNameLength`),
// it returns an error with the reason, so the caller can ask again
func normalizeArchiveName(input string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(input))
	if !archiveNamePattern.MatchString(name) {
		return "", errors.New("use letters, numbers and single hyphens, like a GitHub username")
	}
	if len(name) > maxArchiveNameLength {
		return "", fmt.Errorf("use at most %d characters", maxArchiveNameLength)
	}
	return name, nil
}

// Builds the address of GitHub's page that creates an empty private repository named `repoName`.
// The owner is the account signed in to GitHub (`@me`)
func buildNewDBRepoURL(repoName string) string {
	return "https://github.com/new?" + url.Values{"owner": {"@me"}, "name": {repoName}, "visibility": {"private"}}.Encode()
}

// Builds the address of the fine-grained token creation page, filled in for the DB repository name `repoName`.
// The token name is the repository name (up to 40 characters).
// So even with several archives, GitHub's token list shows which token is for which repository.
// It also fills in the description, the expiration (none) and the needed permissions.
//
// NOTE: The repository to select can't be filled in (docs: there is no such parameter), so the user selects it on the page.
// The resource owner (`target_name`) is not set.
// If an organization were the owner, the token couldn't get the personal account permission this tool uses (Starring read).
// So the default, the user's own account, is right.
// Measured: Contents, Workflows, Secrets, Actions (Read and write) and Starring are filled in.
func buildTokenURL(repoName string) string {
	query := url.Values{
		"name":        {repoName[:min(len(repoName), maxTokenNameLength)]}, // The repository name is ASCII, so cutting by bytes doesn't break characters
		"description": {"starkive dashboard: " + repoName + " and stars"},
		"expires_in":  {"none"},
		"contents":    {"write"},
		"workflows":   {"write"},
		"secrets":     {"write"},
		"actions":     {"write"},
		"starring":    {"read"},
	}
	return "https://github.com/settings/personal-access-tokens/new?" + query.Encode()
}

// Reads the command-line arguments and runs the setup.
// By default it runs on the browser setup page (`webUI`), and with `-headless` in the terminal (`terminalUI`).
// With `-version`, it only prints the version (`printVersion`) and exits.
// It shows the commit to deploy to Vercel as a GitHub address.
// If that commit can't be used (not a git clone, not a public repository, a commit that isn't pushed),
// it shows the reason in the terminal and exits, before the person creates anything.
// On failure, it shows the reason in the terminal (and on the setup page in setup page mode),
// with a note that running it again skips the steps that are done, and exits with code 1.
// On failure it always points to two places to check "if GitHub refused the token" (repository selection and permissions).
// This is because it can't tell for sure which error is that case
func main() {
	headless := flag.Bool("headless", false, "run the setup headless, in the terminal")
	version := flag.Bool("version", false, "print the version information")
	flag.Parse()
	if *version {
		err := printVersion()
		waitForEnterIfConsoleCloses()
		if err != nil {
			os.Exit(1)
		}
		return
	}

	vercelDeployCommit, hasUncommittedChanges, err := resolveVercelDeployCommit()
	if err == nil {
		// Printed before the GitHub check, so the user knows what goes to Vercel even if the check fails
		fmt.Println("Vercel deploy source: " + vercelDeployCommit.buildTreeURL())
		err = checkVercelDeployCommit(readGitHubAPIURL(), vercelDeployCommit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Setup stopped: "+err.Error())
		waitForEnterIfConsoleCloses()
		os.Exit(1)
	}
	if hasUncommittedChanges {
		fmt.Println("Note: this setup tool runs with uncommitted changes. The dashboard is deployed from commit " + vercelDeployCommit.sha[:7] + " on GitHub, without them.")
	}

	server, err := startLocalServer()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Setup stopped: "+err.Error())
		waitForEnterIfConsoleCloses()
		os.Exit(1)
	}
	var u ui
	var web *webUI // Set only in setup page mode. Used to wait until the page fetches the last lines after the setup ends
	if *headless {
		u = newTerminalUI()
		fmt.Println("Starkive Setup Wizard (" + readVersionLabel() + ")")
	} else if web, err = newWebUI(server); err == nil {
		u = web
		launchBrowser(web.startURL)
	} else {
		fmt.Fprintln(os.Stderr, "Setup stopped: "+err.Error())
		waitForEnterIfConsoleCloses()
		os.Exit(1)
	}

	err = run(u, server, vercelDeployCommit)
	if err != nil {
		lines := []string{
			"Setup stopped: " + err.Error(),
			"If GitHub refused the token, check that the token selects the archive repository under Repository access",
			"and has the permissions the token link fills in.",
			"Fix it and run the setup tool again. Steps that are already done are skipped.",
		}
		fmt.Fprintln(os.Stderr)
		for _, line := range lines {
			fmt.Fprintln(os.Stderr, line)
			if web != nil {
				web.say(line)
			}
		}
	}
	if web != nil {
		web.finish(err != nil)
		web.waitUntilSeen(finishGracePeriod)
		if err == nil {
			fmt.Println("Setup finished.")
		}
	}
	server.close()
	if err != nil {
		waitForEnterIfConsoleCloses()
		os.Exit(1)
	}
}

// Runs all the setup steps in order. The steps are the same as in the comment at the top of this file and in `setupSteps`.
//
// Args:
//   - `u`: How to show things to the person and ask them (setup page or terminal)
//   - `server`: The server opened only on this computer. The GitHub App step receives the request coming back from GitHub
//   - `vercelDeployCommit`: The commit to deploy to Vercel as the dashboard (checked with `checkVercelDeployCommit`)
//
// NOTE: The permission checks (Workflows, Actions) run before the GitHub App is created.
// If the App were created while permissions are missing, an unusable App would be left on GitHub.
// The workflow itself is installed last, after the dashboard address is recorded in `config.json`.
// That way, the address to call already exists when the workflow runs for the first time.
func run(u ui, server *localServer, vercelDeployCommit commitRef) error {
	// 1. Archive name, DB repository and its token, repository preparation and sync key.
	// The owner (username) isn't known before the token is entered, so only the repository name is decided first
	u.step(1)
	var archiveName string
	for archiveName == "" {
		input, err := u.ask("Archive name:", "for example, your GitHub username")
		if err != nil {
			return err
		}
		if archiveName, err = normalizeArchiveName(input); err != nil {
			u.say(err.Error())
		}
	}
	repoName := archiveName + dbRepoSuffix
	u.say("Create a repository named `" + repoName + "` for your archive (Private is recommended). If you created it in an earlier run, skip this.")
	u.note("Leave it empty: don't add a readme, `.gitignore` or license, and keep the name `" + repoName + "`. " +
		"The setup looks for it by this name, and a new repository must be empty.")
	u.open("Create the repository", buildNewDBRepoURL(repoName))
	u.say("Next, create a fine-grained personal access token that lets the dashboard read your stars and write to `" + repoName + "`. " +
		"The dashboard keeps it on the server and never shows it in the browser.")
	u.note("The token page is filled in except for one choice: under Repository access, choose Only select repositories and pick `" + repoName + "`. " +
		"Otherwise the token can't reach the repository. Leave the other fields as they are.")
	u.open("Create the token", buildTokenURL(repoName))
	token, err := u.askSecret("Paste the token:")
	if err != nil {
		return err
	}
	if !core.IsValidAccessToken(token) {
		return errors.New("that doesn't look like a GitHub token")
	}
	cfg := &core.Config{
		DBRepoAccessToken: token,
		GitHubAPI:         readGitHubAPIURL(),
		GitHubWeb:         strings.TrimRight(readEnv("STARKIVE_GH_WEB_URL", "https://github.com"), "/"),
	}
	owner, err := core.FetchTokenOwner(cfg)
	if err != nil {
		return err
	}
	u.say("Token owner: `" + owner.Login + "`, the only account that can sign in to the dashboard.")
	public, err := lookUpRepo(cfg, owner.Login+"/"+repoName)
	if err != nil {
		return err
	}
	state, err := core.ResolveDBRepo(cfg)
	if err != nil {
		return err
	}
	if public {
		u.note("This repository is public, so anyone can read your archive. Only you and the dashboard can change it.")
	}
	if err := checkStarring(cfg); err != nil {
		return err
	}
	publicKey, err := readSecretsPublicKey(cfg)
	if err != nil {
		return err
	}
	switch state { // `core.ResolveDBRepo` already returned an error for a repository whose format version differs from this code
	case core.DBRepoForeign:
		return core.ErrDBRepoNotStarkive
	case core.DBRepoEmpty:
		if err := initializeDBRepo(cfg); err != nil {
			return err
		}
		u.say("Initialized the empty repository.")
	default:
		u.say("`" + cfg.DBRepoFullName + "` is already a starkive archive. Setting it up again; your data stays as it is.")
	}
	if err := addInitialFiles(cfg); err != nil {
		return err
	}
	if err := checkWorkflowsPermission(cfg); err != nil { // Check permissions before creating the App
		return err
	}
	if err := checkActionsPermission(cfg); err != nil {
		return err
	}

	// Scheduled sync key.
	// A secret value can't be read back and compared, so it is overwritten every time (the same token gives the same value)
	if err := writeSecret(cfg, publicKey, core.SyncKeySecretName, core.DeriveSyncKey(cfg)); err != nil {
		return err
	}
	u.say("Stored the sync key in the repository's secrets.")

	// 2. Dashboard.
	// It checks with the dashboard's proof (`checkDashboard`) that the deployment at that address uses this repository and the current token.
	// If it passes, there is nothing to do.
	// Otherwise, if there is an App, a registered project exists, so the person only changes its value and redeploys on Vercel.
	// If there is no App, the person deploys a new one on Vercel.
	// If there is no App but the default address answers with another value, it may be a Vercel deployment from an earlier run,
	// or someone else's dashboard may have taken that name.
	// The tool can't tell them apart, so it explains both ways.
	// (If it is from an earlier run, creating another one with the same name doesn't get the default address.)
	u.step(2)
	app, appStatus, err := core.LoadGitHubApp(cfg)
	if err != nil { // If the config file is broken (`core.ErrConfigBroken`), neither the App nor the dashboard address can be used. So stop here, before the person creates anything on GitHub
		return err
	}
	accessKey := core.FormatDBRepoAccessKey(cfg.DBRepoID, token)
	dashboardName := buildDashboardName(owner.Login, archiveName)
	site := app.SiteURL // The canonical address registered in the App. Even an unreadable config keeps its public values (`SiteURL`, `Slug`)
	if site == "" {     // If there is no App, or the saved config has no address, check the default address of the dashboard that the Vercel Deploy link creates
		site = "https://" + dashboardName + ".vercel.app"
	}
	checkErr := checkDashboard(cfg, site)
	switch {
	case checkErr == nil:
		u.say("The dashboard is deployed at `" + site + "`.")
	case appStatus == core.GitHubAppMissing:
		if errors.Is(checkErr, errDashboardOtherValue) { // `errors.Is`: checks wrapped errors too, to see if it is that value
			u.say("A dashboard at `" + site + "` is deployed with another value. If you deployed it in an earlier run, set `" + core.DBRepoAccessKeyEnv +
				"` in its Vercel project's Environment Variables to the value below and redeploy. Otherwise, create a new project.")
		}
		u.say("Create a Vercel project for the dashboard. On Vercel's page, pick the Git account for the dashboard's code. " +
			"If your Vercel account isn't connected to GitHub yet, connect it there first.")
		u.note("Keep the repository name `" + dashboardName + "` and leave the other settings as they are. The setup finds the dashboard by this name.")
		u.open("Create the Vercel project", buildVercelDeployURL(vercelDeployCommit, dashboardName))
		u.say("On that page, under Environment Variables, paste this value as the value of `" + core.DBRepoAccessKeyEnv + "`, then click Vercel's Deploy button. " +
			"It holds your token, so keep it private.")
		u.value(core.DBRepoAccessKeyEnv, accessKey)
		if site, err = askDashboardURL(u, cfg, site, false); err != nil {
			return err
		}
	default: // The registered dashboard is deployed with another value (the token changed, etc.), or doesn't answer now
		u.say("The dashboard at `" + site + "` didn't confirm this token. In the dashboard's Vercel project, open Settings, then Environment Variables, set `" + core.DBRepoAccessKeyEnv + "` to this value, and redeploy.")
		u.value(core.DBRepoAccessKeyEnv, accessKey)
		// If the App is ready, that address is the canonical address, so no other address is accepted.
		// If the App can't be read, step 3 creates a new one with the address it gets here
		if site, err = askDashboardURL(u, cfg, site, appStatus == core.GitHubAppReady); err != nil {
			return err
		}
	}

	// 3. Sign-in GitHub App
	u.step(3)
	if appStatus == core.GitHubAppReady {
		u.say("The sign-in GitHub App is already set up (`" + app.Slug + "`).")
	} else {
		previous := app
		u.say("Next, create a GitHub App that lets you sign in to the dashboard with your GitHub account. It gets no access to your repositories or account data.")
		u.note("On GitHub's page, review the app and click Create GitHub App. Leave the name and the other settings as they are.")
		created, err := createGitHubApp(u, server, cfg, site)
		if err != nil {
			return err
		}
		// If it can't be saved (including when another run saved one first), the App just created isn't used.
		// So tell the person to delete it, and stop.
		// Running again skips this step, because a saved App exists
		if err := core.SaveGitHubApp(cfg, created); err != nil {
			return fmt.Errorf("%w. The app just created isn't used. Delete it: https://github.com/settings/apps/%s", err, created.Slug)
		}
		app = created
		u.say("Created the sign-in app `" + app.Slug + "`.")
		if previous.Slug != "" { // The previous App, which can no longer be read
			u.note("The previous sign-in app `" + previous.Slug + "` is no longer used. Delete it on GitHub.")
			u.link("Open the previous app", "https://github.com/settings/apps/"+previous.Slug)
		}
	}

	// 4. Record the dashboard address and install the workflow if it is missing.
	// The workflow comes last, so the address to call is already recorded when it first runs
	u.step(4)
	if err := finishDBRepo(cfg, app.SiteURL); err != nil {
		return err
	}
	u.say("The automatic sync workflow is set up in `" + cfg.DBRepoFullName + "`.")
	u.done("Sign in to the dashboard with GitHub as `"+owner.Login+"`.", "Open the dashboard", app.SiteURL)
	return nil
}

// Reads the environment variable `key`, or returns `fallback` if it is missing or empty
func readEnv(key string, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Returns the GitHub API address.
// For testing, the same environment variable as the server, `STARKIVE_GH_API_URL`, points it to a fake server
func readGitHubAPIURL() string {
	return strings.TrimRight(readEnv("STARKIVE_GH_API_URL", "https://api.github.com"), "/")
}

// Looks up a repository by name and puts its ID in `cfg.DBRepoID`.
// The server refers to the repository by ID, not by name (the ID stays the same after a rename), so it gets the ID.
//
// Args:
//   - `fullName`: owner/repo
//
// Returns:
//   - `public`: `true` if it is a public repository. Anyone can read the archive in a public repository, so the user is told
//   - If the token can't see it (not created, created with another name, or private and not selected by the token),
//     an error that explains all these cases together.
//     GitHub answers 404 for all of them (docs: a private repository without read access is also 404)
func lookUpRepo(cfg *core.Config, fullName string) (public bool, err error) {
	var repo struct {
		ID      int64 `json:"id"`      // Repository ID
		Private bool  `json:"private"` // Whether it is a private repository
	}
	err = getGitHubJSON(cfg, "/repos/"+fullName, &repo)
	var githubErr *core.GitHubAPIError
	if errors.As(err, &githubErr) && githubErr.Status == http.StatusNotFound { // `errors.As`: if the error is that type, takes it out into the variable (like `dynamic_cast`)
		owner, _, _ := strings.Cut(fullName, "/")
		return false, fmt.Errorf("couldn't find %s with this token (it belongs to %s). Create the repository under that account with exactly the name from step 1, "+
			"and check that the token selects it under Repository access", fullName, owner)
	}
	if err != nil {
		return false, err
	}
	if repo.ID <= 0 {
		return false, errors.New("GitHub returned no repository id")
	}
	cfg.DBRepoID = repo.ID
	return !repo.Private, nil
}

// Checks that the token can read the account's Stars (Starring read permission).
// The dashboard's Import and sync read Stars with this permission.
// Without the permission, it returns GitHub's 403 error as is (the needed permission is added to the message)
func checkStarring(cfg *core.Config) error {
	res, err := core.CallGitHubAPI(cfg, "/user/starred?per_page=1", core.GitHubAPIRequest{})
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return core.CheckGitHubAPI(res)
}

// Opens the link `target` in the operating system's default browser.
// If it can't, it prints the link and asks the person to open it.
func launchBrowser(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil { // `Start`: doesn't wait for it to finish
		fmt.Println("   Couldn't open a browser. Open this link yourself: " + target)
	}
}
