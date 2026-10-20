// The dashboard and sign-in GitHub App steps.
// It takes the address of the deployed dashboard and checks that it is this setup's dashboard (`askDashboardURL`, `checkDashboard`).
// It runs the GitHub App manifest flow through this tool's localhost server (`localServer`, `createGitHubApp`).
// The manifest shape and the code exchange (the parts that talk with GitHub) are in `github_app.go`.
// The Vercel Deploy link and the repeated deployment check are in `vercel.go`.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xorespesp/starkive/core"
)

// How long to wait for the user to create the App on GitHub's page.
// It is shorter than the time GitHub accepts the manifest code (1 hour, docs)
const appCreationTimeout = 30 * time.Minute

// Error for a starkive dashboard that answered, but is deployed with values other than the repository and token being set up.
// Changing the environment variable of that Vercel project and redeploying fixes it
var errDashboardOtherValue = errors.New("the dashboard is deployed with a different " + core.DBRepoAccessKeyEnv +
	". Set it to the value above in Vercel, redeploy, and try again")

// Gets the address of the deployed dashboard.
// If there is a `defaultSite`, it checks that address in the background while waiting (`watchVercelDeployment`).
// When it is confirmed as this setup's dashboard, it moves on without asking.
// If the person chooses to enter it by hand, it stops the check and asks for the address.
// It checks an entered address before using it.
// If the address is malformed or isn't this setup's dashboard, it shows the reason and asks again.
//
// Args:
//   - `u`: Where to ask and show things
//   - `cfg`: The DB repository and token being set up. It checks that the deployment uses these values (`checkDashboard`)
//   - `defaultSite`: The address where the dashboard is expected. An empty string if unknown, and then it asks for the address from the start
//   - `registered`: Whether `defaultSite` is the canonical address registered in the sign-in GitHub App. If so, it doesn't accept another address, even one that passes the check
//
// Returns:
//   - The confirmed address (in the form of `normalizeDashboardURL`). An error if it can't ask anymore (the terminal input ended)
//
// NOTE: The person finishes the deployment on Vercel's page, so the tool can't know when it ends.
// So it checks a known address in the background again and again.
// Entering the address by hand is for the case where Vercel gave another address.
// So when the person chooses that, the check stops.
// The canonical address doesn't change (the sign-in callback and the workflow use it).
// So if an address is registered, an address entered by hand must be that address too.
func askDashboardURL(u ui, cfg *core.Config, defaultSite string, registered bool) (string, error) {
	const prompt = "Dashboard address:"
	const hint = "the production domain under the Vercel project's Domains"
	const manualEntryLabel = "Or enter the deployed dashboard's Vercel domain manually"
	var confirmedVercelDeploySite <-chan string // Channel that receives the address confirmed in the background. `nil` if it doesn't check
	stopWatching := func() {}
	if defaultSite != "" {
		confirmedVercelDeploySite, stopWatching = watchVercelDeployment(cfg, defaultSite)
	}
	defer stopWatching() // `defer`: stops the check when this function ends (it is fine to call it again after it stopped)
	for {
		var input string
		if confirmedVercelDeploySite != nil {
			waiting := "Waiting for your dashboard at `" + defaultSite + "`. The setup continues on its own once it's deployed with the value above."
			value, result, err := u.waitForValue(waiting, prompt, manualEntryLabel, confirmedVercelDeploySite)
			if err != nil {
				return "", err
			}
			switch result {
			case receivedValue: // `watchVercelDeployment` already confirmed it
				return value, nil
			case switchedToTyping: // The person chose to enter it by hand. Stop the check, and only ask from here on
				stopWatching()
				confirmedVercelDeploySite = nil
				continue
			}
			input = value // An address typed in the terminal while waiting
		} else {
			value, err := u.ask(prompt, hint)
			if err != nil {
				return "", err
			}
			input = value
		}
		if input == "" {
			continue
		}
		site, err := normalizeDashboardURL(input)
		if err == nil && registered && site != core.NormalizeSiteURL(defaultSite) {
			err = fmt.Errorf("the sign-in app is registered for %s, so the dashboard must answer there. Set the value above in that Vercel project and redeploy", defaultSite)
		}
		if err == nil {
			err = checkDashboard(cfg, site)
		}
		if err == nil {
			return site, nil
		}
		u.say(err.Error())
	}
}

// Normalizes the dashboard address the user entered into the form `scheme://host`.
// It accepts only https, and accepts http only for a local dev server (localhost, 127.0.0.1).
// It doesn't accept an address with a path or a query (a trailing `/` is removed).
//
// Args:
//   - `input`: The address the user entered
//
// Returns:
//   - The normalized address. The host is lowercase and has no default port (`:443`, `:80`) (`core.NormalizeSiteURL`)
//
// NOTE: This address becomes the App's homepage and sign-in callback.
// When sign-in starts, the server compares the request address with this address.
// Browsers lowercase the host and drop the default port in requests.
// So if the address were saved as typed (uppercase, `:443`), the comparison would always fail and sign-in could never start.
func normalizeDashboardURL(input string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(input), "/"))
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", errors.New("enter an address like https://you-starkive.vercel.app")
	}
	hostname := strings.ToLower(u.Hostname())
	local := hostname == "localhost" || hostname == "127.0.0.1"
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", errors.New("the address must start with https://")
	}
	return core.NormalizeSiteURL(u.Scheme + "://" + u.Host), nil
}

// Checks that the deployment at the address is this setup's starkive dashboard.
// The proof from `GET /api/check-deployment?nonce=` (`core.SignDeploymentNonce`) must match the value computed from the repository ID and token being set up.
// The major format version must also match this tool.
//
// Args:
//   - `site`: An address normalized by `normalizeDashboardURL`
//
// Returns:
//   - An error that says how to fix it, if the check fails.
//     If the dashboard answers with an error (for example, a missing environment variable), the error includes that message as is.
//     If it is a starkive dashboard with a different proof, the error wraps `errDashboardOtherValue` (tell it apart with `errors.Is`)
//
// NOTE: If the proof matches, the deployment's environment variable is exactly this repository and token.
// A dashboard may be deployed with another repository's value, or with the value from before a token change.
// If it were registered, sign-in and the scheduled sync would look at the wrong repository or fail.
// So it stops here.
// It doesn't follow redirects, and rejects them.
// The addresses Vercel gives to each deployment (`*-<hash>.vercel.app`) are behind login protection and redirect to Vercel's sign-in page (measured).
// If such an address were registered in the App, both GitHub's sign-in callback and the workflow's scheduled calls would be blocked.
func checkDashboard(cfg *core.Config, site string) error {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	nonce := hex.EncodeToString(random) // A new value for every check, so an old answer can't be reused
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, // Returns the redirect response as is
	}
	res, err := client.Get(site + "/api/check-deployment?nonce=" + nonce)
	if err != nil {
		return fmt.Errorf("couldn't reach %s: %v", site, err)
	}
	defer res.Body.Close()
	var body struct {
		Format core.DBRepoFormatVersion `json:"format"` // The dashboard's format version
		Proof  string                   `json:"proof"`  // The value the dashboard signed `nonce` with, using its environment variable
		Error  string                   `json:"error"`  // An error answer from the dashboard API, `{"error": ...}`
	}
	decoded := json.NewDecoder(res.Body).Decode(&body) == nil
	switch {
	case res.StatusCode >= 300 && res.StatusCode < 400:
		return fmt.Errorf("%s redirects (probably to Vercel's sign-in). Use the production domain listed under the Vercel project's Domains", site)
	case res.StatusCode == http.StatusOK && decoded && body.Proof != "":
		if !body.Format.IsCompatible() {
			return fmt.Errorf("the dashboard runs starkive format %s, but this setup tool uses format %s. Use the same version of starkive for both, redeploy, and try again", body.Format, core.DBRepoFormat)
		}
		if !hmac.Equal([]byte(body.Proof), []byte(core.SignDeploymentNonce(cfg, nonce))) {
			return fmt.Errorf("%s: %w", site, errDashboardOtherValue) // `%w`: lets `errors.Is` find the wrapped error
		}
		return nil
	case decoded && body.Error != "":
		return fmt.Errorf("the dashboard answered: %s", body.Error)
	}
	return fmt.Errorf("%s doesn't look like a starkive dashboard of this version (HTTP %d). Wait for the deployment to finish, check the address, or redeploy the dashboard", site, res.StatusCode)
}

// Creates the sign-in GitHub App with the GitHub App manifest flow.
//  1. Adds the App creation start page (`/github-app`) to this tool's server and opens it. That page sends the manifest to GitHub's App creation page with a form POST
//  2. The user reviews it on GitHub's page and clicks "Create GitHub App"
//  3. GitHub sends the user back to `/github-app/callback?code&state`. The tool checks the state and turns the code into App details (`createGitHubAppFromCode`, including the owner check)
//
// Args:
//   - `u`: Where to open the start page
//   - `server`: The server opened only on this computer. It serves the start page and receives the returning request
//   - `site`: The dashboard address. It becomes the App's homepage and sign-in callback
//
// Returns:
//   - The created App. An error if the user doesn't create it within 30 minutes (`appCreationTimeout`)
//
// NOTE: The App's homepage and sign-in callback are the remote dashboard address.
// Only the return address after creation (`redirect_url`) is localhost.
// GitHub accepts this mix (measured).
// A GitHub App can't be created silently through the API, and the user always confirms it on GitHub's page.
// The state is the value that checks that the start page and the returning request belong to this run.
// These two paths don't look at the setup page's key cookie (terminal mode has no cookie).
// It adds paths to the server, so call it only once per run.
func createGitHubApp(u ui, server *localServer, cfg *core.Config, site string) (core.GitHubApp, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return core.GitHubApp{}, err
	}
	state := hex.EncodeToString(random)
	manifest, err := newGitHubAppManifest(cfg.DBRepoFullName, site, server.base+"/github-app/callback")
	if err != nil {
		return core.GitHubApp{}, err
	}

	// The result that the handler of the returning request passes to the waiting side
	type outcome struct {
		app core.GitHubApp // The created App
		err error          // Failure of the code exchange or the owner check
	}
	done := make(chan outcome, 1) // Buffer of 1: after the first result is put in, the sender doesn't block even if the receiver isn't waiting yet
	// Start page: puts the manifest in a hidden input and sends it to GitHub right away with a form POST (GitHub accepts the manifest only by form POST)
	server.handle("/github-app", func(w http.ResponseWriter, r *http.Request) {
		if !hmac.Equal([]byte(r.URL.Query().Get("state")), []byte(state)) { // `hmac.Equal`: constant-time comparison
			http.Error(w, "This page wasn't opened by this setup run.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>starkive setup</title><p>Opening GitHub…</p>
<form id="f" method="post" action="%s"><input type="hidden" name="manifest" value="%s"></form>
<script>document.getElementById('f').submit()</script>`, html.EscapeString(buildGitHubAppCreateURL(cfg, state)), html.EscapeString(manifest))
	})
	// Where GitHub sends the user back after creating the App.
	// A request with a different state or no code has nothing to do with this run, so it is rejected
	server.handle("/github-app/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if !hmac.Equal([]byte(query.Get("state")), []byte(state)) || query.Get("code") == "" {
			http.Error(w, "This page wasn't opened by this setup run.", http.StatusBadRequest)
			return
		}
		app, err := createGitHubAppFromCode(cfg, query.Get("code"))
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err != nil {
			fmt.Fprintln(w, "Creating the app failed. Close this tab and see the setup tool.")
		} else {
			fmt.Fprintln(w, "The app is created. Close this tab and continue in the setup tool.")
		}
		select { // Passes on only the first result
		case done <- outcome{app: app, err: err}:
		default: // There is already a result (the same callback address was opened again)
		}
	})

	u.open("Create the GitHub App", server.base+"/github-app?state="+state)
	select { // Waits for whichever comes first: the result of the returning request or the timeout
	case result := <-done:
		return result.app, result.err
	case <-time.After(appCreationTimeout):
		return core.GitHubApp{}, errors.New("timed out waiting for GitHub. Run the setup tool again")
	}
}
