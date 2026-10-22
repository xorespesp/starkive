// Sign-in with a GitHub account (OAuth web flow).
// Only one person can enter the dashboard: the owner of the server token.
// The flow has three steps:
//  1. `StartGitHubLogin`: Makes an anti-forgery value (state), puts it in a short signed cookie, and returns the GitHub authorization address
//  2. When the user approves on GitHub, GitHub sends them back to the callback registered in the App (`/api/login-session`) with `code` and `state`
//  3. `FinishGitHubLogin`: Checks the state, exchanges `code` for a user token, and checks whether that user's ID is the token owner
//
// The server stores nothing.
// The browser carries the state in a signed cookie, and the sign-in result in the session cookie (`session.go`).
// The settings of the sign-in GitHub App are in `sign_in_app.go`.
package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Name, lifetime and attributes of the sign-in state cookie
const (
	githubLoginStateCookie = "github_login_state" // Cookie name
	githubLoginStateTTL    = 10 * time.Minute     // How long to wait between starting sign-in and coming back from GitHub
	// Cookie attributes. `Path` is narrowed to the sign-in API, so the cookie is not sent with other requests.
	// `SameSite=Lax`: the navigation back from GitHub comes from another site, so a `Strict` cookie would not be sent.
	// A `Lax` cookie is sent with this kind of GET navigation
	githubLoginStateAttrs = "Path=/api/login-session; HttpOnly; Secure; SameSite=Lax"
)

// `Set-Cookie` value that deletes the state cookie.
// The callback always sends it, whatever the result, so opening the same callback address again doesn't work
const ClearGitHubLoginStateCookie = githubLoginStateCookie + "=; Max-Age=0; " + githubLoginStateAttrs

// Reason a GitHub sign-in failed.
// The callback passes it to the page as `/?login_error=<reason>`, and the page picks the matching text
type GitHubLoginFailure string

// Reasons a GitHub sign-in failed
const (
	GitHubLoginCancelled  GitHubLoginFailure = "cancelled"   // The user didn't approve on the GitHub page
	GitHubLoginExpired    GitHubLoginFailure = "expired"     // The state is missing, different or expired (10 minutes passed, or a forged request)
	GitHubLoginNotAllowed GitHubLoginFailure = "not_allowed" // The signed-in GitHub account is not the token owner
	GitHubLoginFailed     GitHubLoginFailure = "failed"      // Talking to GitHub failed (including when the token owner couldn't be checked)
)

// Starts a GitHub sign-in: makes a state, puts it in a cookie, and builds the GitHub authorization address for the browser to go to.
//
// Args:
//   - `app`: The sign-in GitHub App (its `client_id` is used)
//   - `now`: The current time. The state expiry is counted from here
//
// Returns:
//   - `authorizeURL`: The GitHub authorization address for the browser to go to
//   - `setCookie`: The `Set-Cookie` value of the state cookie, to add to the response
//
// NOTE: The callback address (`redirect_uri`) is not sent.
// GitHub sends the user back to the callback address registered in the App (docs), so the request's `Host` header doesn't need to be trusted.
func StartGitHubLogin(cfg *EnvConfig, app GitHubApp, now time.Time) (authorizeURL string, setCookie string, err error) {
	state, cookieValue, err := newLoginState(cfg, now.Add(githubLoginStateTTL))
	if err != nil {
		return "", "", err
	}
	cookie := fmt.Sprintf("%s=%s; Max-Age=%d; %s", githubLoginStateCookie, cookieValue, int(githubLoginStateTTL.Seconds()), githubLoginStateAttrs)
	query := url.Values{"client_id": {app.ClientID}, "state": {state}, "allow_signup": {"false"}}
	return cfg.GitHubWeb + "/login/oauth/authorize?" + query.Encode(), cookie, nil
}

// Checks the callback request from GitHub and decides whether to allow the sign-in.
// Order of checks: refusal on the GitHub page -> state (forgery, expiry) -> exchange `code` for a user token -> that user's ID -> compare with the token owner.
//
// Args:
//   - `app`: The sign-in GitHub App (the code exchange uses its `client_id` and client secret)
//   - `r`: The callback request. Reads `code`, `state` and `error` from the address, and the state cookie
//   - `now`: The current time. Used to check expiry of the state
//
// Returns:
//   - The failure reason. Empty if the user is the token owner
//   - Details for the server log (when the reason alone is not enough). Never sent to the page
func FinishGitHubLogin(cfg *EnvConfig, app GitHubApp, r *http.Request, now time.Time) (GitHubLoginFailure, error) {
	ctx := r.Context() // The deadline that `ServeAPIRequest` set for this request
	query := r.URL.Query()
	if query.Get("error") != "" {
		return GitHubLoginCancelled, nil
	}
	if !isValidLoginState(cfg, r, query.Get("state"), now) {
		return GitHubLoginExpired, nil
	}
	token, err := exchangeGitHubLoginCode(ctx, cfg, app, query.Get("code"))
	if err != nil {
		return GitHubLoginFailed, err
	}
	userID, err := fetchGitHubUserID(ctx, cfg, token)
	if err != nil {
		return GitHubLoginFailed, err
	}
	owner, err := FetchTokenOwner(ctx, cfg)
	if err != nil {
		return GitHubLoginFailed, err
	}
	if !IsGitHubLoginAllowed(owner.ID, userID) {
		return GitHubLoginNotAllowed, fmt.Errorf("GitHub user %d is not the token owner %d", userID, owner.ID)
	}
	return "", nil
}

// Owner (GitHub account) of the server token.
// The only person who can sign in to the dashboard
type GitHubUser struct {
	ID    int64  `json:"id"`    // User ID. Names can change, so the sign-in check uses the ID
	Login string `json:"login"` // Username
}

// In-memory cache of the token owner, per instance.
// The key is `buildEnvCacheKey` (the same token gives the same key)
var tokenOwnerCache cache[GitHubUser]

// How long to keep the token owner.
// The owner of a token doesn't change, so it is kept for a long time
const tokenOwnerTTL = 24 * time.Hour

// Reads the owner of the server token from GitHub (`GET /user`).
// Only a successful result is kept in `tokenOwnerCache`.
// After a failure, it asks again next time.
//
// Returns:
//   - The token owner. A 502 `HTTPError` if GitHub returns a user with no ID or name
//
// NOTE: The owner of a token stays the same until the token changes.
// So it is kept, and GitHub is not asked again on every call.
// The owner's username can still change, so a kept owner can have an old username for up to `tokenOwnerTTL`.
func FetchTokenOwner(ctx context.Context, cfg *EnvConfig) (GitHubUser, error) {
	if owner, found := tokenOwnerCache.get(buildEnvCacheKey(cfg)); found {
		return owner, nil
	}
	res, err := CallGitHubAPI(ctx, cfg, "/user", GitHubAPIRequest{})
	if err != nil {
		return GitHubUser{}, err
	}
	defer res.Body.Close()
	if err := CheckGitHubResponse(res); err != nil {
		return GitHubUser{}, err
	}
	var owner GitHubUser
	if err := DecodeGitHubResponse(res, &owner); err != nil {
		return GitHubUser{}, err
	}
	if owner.ID <= 0 || owner.Login == "" {
		return GitHubUser{}, Errorf(http.StatusBadGateway, "GitHub returned no user for the token")
	}
	tokenOwnerCache.put(buildEnvCacheKey(cfg), owner, tokenOwnerTTL)
	return owner, nil
}

// Decides whether a GitHub user may enter the dashboard (whether they are the owner of the server token).
//
// Args:
//   - `tokenOwnerID`: User ID of the server token owner. 0 if unknown
//   - `githubUserID`: ID of the user who signed in on GitHub
//
// NOTE: The sign-in check is made only in this function.
// If the token owner is unknown (0), nobody is allowed.
// There must be no path where "unknown" turns into "anyone is allowed".
func IsGitHubLoginAllowed(tokenOwnerID int64, githubUserID int64) bool {
	return tokenOwnerID > 0 && githubUserID == tokenOwnerID
}

// Purpose label added in front of the signed data when signing the state cookie
//
// NOTE: The state cookie is signed with the same key as the session cookie (`signWithSessionKey`).
// The label makes the signed data different, so a state cookie signature can't be used as a session cookie.
const loginStatePurpose = "github-login-state:"

// Makes a new anti-forgery value (state).
//
// Args:
//   - `expires`: When the state expires
//
// Returns:
//   - `state`: Random value to add to the GitHub authorization address (32 bytes written as base64url)
//   - `cookieValue`: Value for the state cookie, `<state>.<expiry (Unix seconds)>.<signature>`
func newLoginState(cfg *EnvConfig, expires time.Time) (state string, cookieValue string, err error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", "", err
	}
	state = base64.RawURLEncoding.EncodeToString(random)
	exp := strconv.FormatInt(expires.Unix(), 10)
	return state, state + "." + exp + "." + signWithSessionKey(cfg, loginStatePurpose+state+"."+exp), nil
}

// Decides whether the state in the callback request belongs to a sign-in started in this browser.
// It checks the signature and expiry of the state cookie (`<state>.<expiry>.<signature>`), and whether it matches the state in the callback address.
//
// Args:
//   - `r`: The callback request. The state cookie is read from it
//   - `state`: The state that came with the callback address
//   - `now`: The current time. Used to check expiry
//
// NOTE: This blocks attacks like login CSRF.
// In such an attack, someone makes you open the callback address of a sign-in they started, so their sign-in result sticks to your browser.
func isValidLoginState(cfg *EnvConfig, r *http.Request, state string, now time.Time) bool {
	cookie, err := r.Cookie(githubLoginStateCookie)
	if err != nil || state == "" {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return false
	}
	saved, exp, sig := parts[0], parts[1], parts[2]
	if !hmac.Equal([]byte(sig), []byte(signWithSessionKey(cfg, loginStatePurpose+saved+"."+exp))) { // `hmac.Equal`: compares in constant time, so the signature can't be guessed from the time it takes
		return false
	}
	expires, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || expires <= now.Unix() {
		return false
	}
	return hmac.Equal([]byte(saved), []byte(state))
}

// Exchanges the callback `code` for a GitHub user token (`POST <GitHubWeb>/login/oauth/access_token`).
//
// Args:
//   - `app`: The sign-in GitHub App (sends its `client_id` and client secret)
//   - `code`: The one-time code that came with the callback address
//
// Returns:
//   - The user token. It is used once to check that user's ID, then thrown away
//
// NOTE: Even when the exchange fails, GitHub answers 200 and puts the reason in `error` in the body (e.g. `bad_verification_code`, docs).
// So failure is detected by an empty token.
func exchangeGitHubLoginCode(ctx context.Context, cfg *EnvConfig, app GitHubApp, code string) (string, error) {
	if code == "" {
		return "", fmt.Errorf("callback has no code")
	}
	form := url.Values{"client_id": {app.ClientID}, "client_secret": {app.ClientSecret}, "code": {code}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.GitHubWeb+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // Without it, GitHub answers in form encoding (docs)
	res, err := httpClient.Do(req)
	if err != nil {
		return "", newGitHubUnreachableError(err)
	}
	defer res.Body.Close()
	if err := CheckGitHubResponse(res); err != nil {
		return "", err
	}
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := DecodeGitHubResponse(res, &body); err != nil {
		return "", err
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("GitHub token exchange failed: %s", body.Error)
	}
	return body.AccessToken, nil
}

// Reads the GitHub ID of a user with that user's token (`GET /user`).
//
// Args:
//   - `token`: The user token from `exchangeGitHubLoginCode`.
//     The sign-in App has no permissions, so this token can only read the public profile
func fetchGitHubUserID(ctx context.Context, cfg *EnvConfig, token string) (int64, error) {
	res, err := CallGitHubAPI(ctx, cfg, "/user", GitHubAPIRequest{Token: token})
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if err := CheckGitHubResponse(res); err != nil {
		return 0, err
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := DecodeGitHubResponse(res, &user); err != nil {
		return 0, err
	}
	return user.ID, nil
}
