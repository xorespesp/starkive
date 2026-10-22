// `/api/login-session`: API to sign in to the dashboard with a GitHub account, check the sign-in state, and sign out.
//
//	GET              -> { repo, username }: `repo` is the DB repository `owner/repo`, `username` is the username of the account that signed in
//	                 (an empty string if it can't be read).
//	                 401 if not signed in. 409 if the DB repository can't be used.
//	                 The sync status is in `/api/sync-status`
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
	"context"
	"errors"
	"log"
	"net/http"
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
	ctx := r.Context()
	query := r.URL.Query()
	if query.Has("code") || query.Has("error") {
		return handleLoginSessionCallback(w, r)
	}
	dbRepo, err := core.Authorize(r)
	if err != nil {
		return err
	}
	// Only the token owner can sign in, so this is the account that signed in.
	// The username is only shown, so failing to read it doesn't keep the dashboard from opening
	username := ""
	if owner, err := core.FetchTokenOwner(ctx, dbRepo.EnvConfig()); err == nil {
		username = owner.Login
	} else {
		log.Printf("token owner: %v", err)
	}
	core.WriteAPIJSONResponse(w, http.StatusOK, loginSessionStatus{Repo: dbRepo.FullName(), Username: username})
	return nil
}

// Body of the sign-in state response (`GET /api/login-session`)
type loginSessionStatus struct {
	Repo     string `json:"repo"`     // DB repository `owner/repo`
	Username string `json:"username"` // Username of the account that signed in. Empty if it can't be read
}

// Reads the sign-in GitHub App settings from the DB repository.
//
// Returns:
//   - The App settings. If they can't be used, a 409 `HTTPError` that tells you to run the setup tool.
//     This covers: not created yet, can't decrypt because the server token changed, and a broken config file
//
// NOTE: It doesn't tell these causes apart.
// In every case, the fix is to run the setup tool, and the tool reports the exact cause.
func loadSignInApp(ctx context.Context, dbRepo *core.DBRepo) (core.GitHubApp, error) {
	app, status, err := core.LoadGitHubApp(ctx, dbRepo)
	if err != nil && !errors.Is(err, core.ErrDBConfigFileBroken) {
		return core.GitHubApp{}, err
	}
	if err != nil || status != core.GitHubAppReady { // A broken config file (`core.ErrDBConfigFileBroken`) gets the same answer
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
	ctx := r.Context()
	if err := core.RequireSameOrigin(r); err != nil {
		return err
	}
	dbRepo, err := core.OpenDBRepoFromEnv(ctx)
	if err != nil {
		return err
	}
	app, err := loadSignInApp(ctx, dbRepo)
	if err != nil {
		return err
	}
	if core.ReadAPIRequestSiteURL(r) != core.NormalizeSiteURL(app.SiteURL) {
		core.WriteAPIJSONResponse(w, http.StatusConflict, map[string]string{"error": "Sign in at " + app.SiteURL, "dashboard_url": app.SiteURL})
		return nil
	}
	authorizeURL, stateCookie, err := core.StartGitHubLogin(dbRepo.EnvConfig(), app, time.Now())
	if err != nil {
		return err
	}
	w.Header().Add("Set-Cookie", stateCookie)
	core.WriteAPIJSONResponse(w, http.StatusOK, map[string]string{"url": authorizeURL})
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
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Set-Cookie", core.ClearGitHubLoginStateCookie) // Set first, so the error answer below deletes it too
	dbRepo, err := core.OpenDBRepoFromEnv(ctx)
	if err != nil {
		return err
	}
	failure := core.GitHubLoginFailed // If the App settings can't be read, it ends with this reason
	app, detail := loadSignInApp(ctx, dbRepo)
	if detail == nil {
		failure, detail = core.FinishGitHubLogin(dbRepo.EnvConfig(), app, r, time.Now())
	}
	if failure != "" {
		log.Printf("GitHub login failed (%s): %v", failure, detail)
		http.Redirect(w, r, "/?login_error="+string(failure), http.StatusFound)
		return nil
	}
	w.Header().Add("Set-Cookie", core.NewSessionCookie(dbRepo.EnvConfig(), time.Now()))
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
	core.WriteAPIJSONResponse(w, http.StatusOK, map[string]bool{"ok": true})
	return nil
}

// Entry point for `/api/login-session` requests. It sends each method to its handler.
// Vercel finds the exported function in this file and deploys it as one function.
// `core.ServeAPIRequest` turns an error from a handler into a JSON error response.
// Other methods get 405.
func HandleLoginSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		core.ServeAPIRequest(w, r, handleLoginSessionGet)
	case http.MethodPost:
		core.ServeAPIRequest(w, r, handleLoginSessionPost)
	case http.MethodDelete:
		core.ServeAPIRequest(w, r, handleLoginSessionDelete)
	default:
		core.WriteAPIMethodNotAllowed(w)
	}
}
