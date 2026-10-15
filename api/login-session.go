// `/api/login-session`: API to sign in to the dashboard with a GitHub account, check the sign-in state, and sign out.
//
//	GET              -> { repo, sync, sync_error? }. 401 if not signed in. 409 if the DB repository can't be used.
//	                 `repo` is the DB repository `owner/repo`. `sync` is the auto sync status (`core.SyncStatus`).
//	                 If the auto sync status can't be read, `sync` is `null` and `sync_error` holds the reason (`formatSyncReadError`)
//	POST             -> { url }: The GitHub authorization address the browser goes to. Also sets the state cookie.
//	                 409 { error } if the sign-in GitHub App is missing, can't be decrypted, or `config.json` is broken (tells you to run the setup tool).
//	                 409 { error, dashboard_url } if called from an address that isn't the dashboard's canonical address (the page moves there)
//	GET ?code&state  The callback GitHub sends the browser back to after sign-in.
//	                 For the token owner, sets a session cookie (about 6 months) and redirects to `/`.
//	                 Otherwise redirects to `/?login_error=<reason>` (the reason is a `core.GitHubLoginFailure`)
//	DELETE           -> { ok: true }: Deletes the session cookie (sign out)
//
// The server takes no password, so there is nothing to brute-force.
// GitHub handles account security (two-factor auth, passkeys).
// This server only checks that the user ID GitHub confirmed is the owner of the server token (`core.FinishGitHubLogin`).

package handler

import (
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/xorespesp/starkive/core"
)

// Answers with the sign-in state.
// If the address has `code` or `error`, it is the callback from GitHub, so it goes to `handleLoginSessionCallback`.
// Answers 401 if not signed in, and 409 if the DB repository can't be used.
// The page uses this answer to decide between opening the dashboard and showing the sign-in screen.
//
// NOTE: The callback comes as a GET to the same address.
// This is because the GitHub App has only one registered callback address, `/api/login-session`.
func handleLoginSessionGet(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query()
	if query.Has("code") || query.Has("error") {
		return handleLoginSessionCallback(w, r)
	}
	cfg, err := core.Authorize(r)
	if err != nil {
		return err
	}
	// The auto sync status is not needed to open the dashboard, so the dashboard still opens if only this can't be read.
	// The reason is sent along, so the failure doesn't look like "no problem" (the page shows a warning)
	response := loginSessionStatus{Repo: cfg.DBRepoFullName}
	if status, err := core.ReadSyncStatus(cfg); err == nil {
		response.Sync = &status
	} else {
		log.Printf("sync status: %v", err)
		response.SyncError = formatSyncReadError(err)
	}
	core.WriteJSON(w, http.StatusOK, response)
	return nil
}

// Body of the sign-in state response (`GET /api/login-session`)
type loginSessionStatus struct {
	Repo      string           `json:"repo"`                 // DB repository `owner/repo`. Used for the repository link on the page
	Sync      *core.SyncStatus `json:"sync"`                 // Auto sync status. A pointer: `nil` if it can't be read, so it becomes `null` in JSON
	SyncError string           `json:"sync_error,omitempty"` // Why the auto sync status couldn't be read (`formatSyncReadError`). Empty if it was read, so it's left out of the JSON
}

// Turns the error `err` from reading the auto sync status into the text of `sync_error`.
// A failure that GitHub answered (`core.GitHubAPIError`) and our own message (`core.HTTPError`) are shown as they are.
// Any other error becomes a general sentence, because it is an internal detail (like in `core.Respond`).
// The details stay in the server log.
//
// NOTE: `core.ReadSyncStatus` joins up to three errors with `errors.Join`, so each one is judged on its own.
// `Unwrap() []error` is the method that such a joined error has.
func formatSyncReadError(err error) string {
	parts := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok { // `err.(T)`: checks whether the value has those methods (like `dynamic_cast`)
		parts = joined.Unwrap()
	}
	lines := []string{}
	for _, part := range parts {
		var githubErr *core.GitHubAPIError
		var httpErr *core.HTTPError
		line := "Internal server error"
		switch {
		case errors.As(part, &githubErr): // `errors.As`: if the error is that type, takes it out into the variable
			line = githubErr.Error()
		case errors.As(part, &httpErr):
			line = httpErr.Message
		}
		if !slices.Contains(lines, line) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// Reads the sign-in GitHub App settings from the DB repository.
//
// Returns:
//   - The App settings. If they can't be used, a 409 `HTTPError` that tells you to run the setup tool.
//     This covers: not created yet, can't decrypt because the server token changed, and a broken config file
//
// NOTE: It doesn't tell these causes apart.
// In every case, the fix is to run the setup tool, and the tool reports the exact cause.
func loadSignInApp(cfg *core.Config) (core.GitHubApp, error) {
	app, status, err := core.LoadGitHubApp(cfg)
	if err != nil && !errors.Is(err, core.ErrConfigBroken) {
		return core.GitHubApp{}, err
	}
	if err != nil || status != core.GitHubAppReady { // A broken config file (`core.ErrConfigBroken`) gets the same answer
		return core.GitHubApp{}, core.Errorf(http.StatusConflict, "Sign-in isn't set up for this deployment. Run the starkive setup tool")
	}
	return app, nil
}

// Starts a GitHub sign-in.
// Sets a cookie with an anti-forgery value (state), and answers `{ url }` with the GitHub authorization address for the browser.
// Answers 403 for a request from another site, and 409 if the App settings can't be used.
// Answers 409 `{ error, dashboard_url }` if called from an address that isn't the canonical address.
//
// NOTE: GitHub sends the sign-in result only to the address registered in the App, which is the dashboard's canonical address (docs).
// The browser sends the state cookie only to the address that set it.
// Vercel gives one project several addresses (docs).
// So a sign-in started at another address always fails, because the callback has no cookie.
// That's why it stops before starting and tells the browser the canonical address.
// The canonical address is a public dashboard address, so it's fine to reveal it before sign-in.
// The comparison ignores differences in letter case and default port (`core.NormalizeSiteURL`).
func handleLoginSessionPost(w http.ResponseWriter, r *http.Request) error {
	if err := core.RequireSameOrigin(r); err != nil {
		return err
	}
	cfg, err := core.LoadConfig()
	if err != nil {
		return err
	}
	app, err := loadSignInApp(cfg)
	if err != nil {
		return err
	}
	if core.ReadSiteURL(r) != core.NormalizeSiteURL(app.SiteURL) {
		core.WriteJSON(w, http.StatusConflict, map[string]string{"error": "Sign in at " + app.SiteURL, "dashboard_url": app.SiteURL})
		return nil
	}
	authorizeURL, stateCookie, err := core.StartGitHubLogin(cfg, app, time.Now())
	if err != nil {
		return err
	}
	w.Header().Add("Set-Cookie", stateCookie)
	core.WriteJSON(w, http.StatusOK, map[string]string{"url": authorizeURL})
	return nil
}

// Handles the callback GitHub sends after sign-in.
// For the token owner, it sets a session cookie and redirects to `/`.
// Otherwise it redirects to `/?login_error=<reason>`.
// The request comes from a page navigation in the browser, so it redirects to a page instead of answering JSON.
// It answers with a JSON error only when the environment variable or the DB repository has a problem.
//
// NOTE: The state cookie is deleted whatever the result.
// This way, opening the same callback address again doesn't work.
// The details of a failure (GitHub errors, etc.) go only to the server log.
// The page gets only the kind of reason.
func handleLoginSessionCallback(w http.ResponseWriter, r *http.Request) error {
	cfg, err := core.LoadConfig()
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Set-Cookie", core.ClearGitHubLoginStateCookie)
	failure := core.GitHubLoginFailed // If the App settings can't be read, it ends with this reason
	app, detail := loadSignInApp(cfg)
	if detail == nil {
		failure, detail = core.FinishGitHubLogin(cfg, app, r, time.Now())
	}
	if failure != "" {
		log.Printf("GitHub login failed (%s): %v", failure, detail)
		http.Redirect(w, r, "/?login_error="+string(failure), http.StatusFound)
		return nil
	}
	w.Header().Add("Set-Cookie", core.NewSessionCookie(cfg, time.Now()))
	http.Redirect(w, r, "/", http.StatusFound)
	return nil
}

// Signs out: sends a header that deletes the session cookie, and answers `{ ok: true }`.
// Answers 403 for a request from another site.
// The server keeps no list of sessions, so the only thing to delete is this browser's cookie.
func handleLoginSessionDelete(w http.ResponseWriter, r *http.Request) error {
	if err := core.RequireSameOrigin(r); err != nil {
		return err
	}
	w.Header().Add("Set-Cookie", core.ClearSessionCookie)
	core.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// Entry point for `/api/login-session` requests. It sends each method to its handler.
// Vercel finds the exported function in this file and deploys it as one function.
// `core.Respond` turns an error from a handler into a JSON error response.
// Other methods get 405.
func HandleLoginSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		core.Respond(w, r, handleLoginSessionGet)
	case http.MethodPost:
		core.Respond(w, r, handleLoginSessionPost)
	case http.MethodDelete:
		core.Respond(w, r, handleLoginSessionDelete)
	default:
		core.WriteMethodNotAllowed(w)
	}
}
